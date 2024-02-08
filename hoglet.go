package hoglet

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// Circuit wraps a function and behaves like a simple circuit and breaker: it opens when the wrapped function fails and
// stops calling the wrapped function until it closes again, returning [ErrCircuitOpen] in the meantime.
//
// A zero Circuit will panic, analogous to calling a nil function variable. Initialize with [NewCircuit].
type Circuit[IN, OUT any] struct {
	f WrappedFunc[IN, OUT]
	options

	// State

	openedAt atomic.Int64 // unix microseconds
}

// options is a sub-struct to avoid requiring type parameters in the [Option] type.
type options struct {
	// isFailure is a filter function that determines whether an error can open the breaker.
	isFailure func(error) bool

	// halfOpenDelay is the duration the circuit will stay open before switching to the half-open state, where a
	// limited (~1) amount of calls are allowed that - if successful - may re-close the breaker.
	halfOpenDelay time.Duration

	breaker         Breaker
	observerFactory ObserverFactory
}

// Breaker is the interface implemented by the different breakers, responsible for actually opening the circuit.
// Each implementation behaves differently when deciding whether to open the circuit upon failure.
type Breaker interface {
	// observe updates the breaker's state and returns whether the circuit should change state.
	// The halfOpen parameter indicates whether the call was made in half-open state.
	// The failure parameter indicates whether the call failed.
	observe(halfOpen, failure bool) stateChange

	Option // breakers can also modify or sanity-check their circuit's options
}

// ObserverFactory is an interface that allows customizing the per-call observer creation.
type ObserverFactory interface {
	// ObserverForCall returns an [Observer] for the incoming call.
	// It is called with the current [State] of the circuit, before calling the wrapped function.
	ObserverForCall(context.Context, State) (Observer, error)
}

// BreakerMiddleware wraps an [ObserverFactory] and returns a new [ObserverFactory].
type BreakerMiddleware interface {
	Wrap(ObserverFactory) (ObserverFactory, error)
}

type BreakerMiddlewareFunc func(ObserverFactory) (ObserverFactory, error)

func (f BreakerMiddlewareFunc) Wrap(of ObserverFactory) (ObserverFactory, error) {
	return f(of)
}

// WrappedFunc is the type of the function wrapped by a Breaker.
type WrappedFunc[IN, OUT any] func(context.Context, IN) (OUT, error)

// dedupObservableCall wraps an [Observer] ensuring it can only be observed a single time.
func dedupObservableCall(obs Observer) Observer {
	return &dedupedObserver{Observer: obs}
}

type dedupedObserver struct {
	Observer
	o sync.Once
}

func (d *dedupedObserver) Observe(failure bool) {
	d.o.Do(func() {
		d.Observer.Observe(failure)
	})
}

// NewCircuit instantiates a new [Circuit] that wraps the provided function. See [Circuit.Call] for calling semantics.
// A Circuit with a nil breaker is a noop wrapper around the provided function and will never open.
func NewCircuit[IN, OUT any](f WrappedFunc[IN, OUT], breaker Breaker, opts ...Option) (*Circuit[IN, OUT], error) {
	c := &Circuit[IN, OUT]{
		f: f,
	}

	o := options{
		isFailure: defaultFailureCondition,
		breaker:   noopBreaker{},
	}

	if breaker != nil {
		o.breaker = breaker
		// apply breaker as last, so it can verify
		opts = append(opts, breaker)
	}

	// the default observerFactory is the circuit itself
	o.observerFactory = c

	for _, opt := range opts {
		if err := opt.apply(&o); err != nil {
			return nil, fmt.Errorf("applying option: %w", err)
		}
	}

	c.options = o

	return c, nil
}

// State reports the current [State] of the circuit.
// It should only be used for informational purposes. To minimize race conditions, the circuit should be called directly
// instead of checking its state first.
func (c *Circuit[IN, OUT]) State() State {
	oa := c.openedAt.Load()

	if oa == 0 {
		// closed
		return StateClosed
	}

	if c.halfOpenDelay == 0 || time.Since(time.UnixMicro(oa)) < c.halfOpenDelay {
		// open
		return StateOpen
	}

	// half-open
	return StateHalfOpen
}

// stateForCall returns the state of the circuit meant for the next call.
// It wraps [State] to keep the mutable part outside of the external API.
func (c *Circuit[IN, OUT]) stateForCall() State {
	state := c.State()

	if state == StateHalfOpen {
		// We reset openedAt to block further calls to pass through when half-open. A success will cause the breaker to
		// close. This is slightly racy: multiple goroutines may reach this point concurrently since we do not lock the
		// breaker.
		c.reopen()
	}

	return state
}

// open marks the circuit as open, if it not already.
// It is safe for concurrent calls and only the first one will actually set opening time.
func (c *Circuit[IN, OUT]) open() {
	// CompareAndSwap is needed to avoid clobbering another goroutine's openedAt value.
	c.openedAt.CompareAndSwap(0, time.Now().UnixMicro())
}

// reopen forcefully (re)marks the circuit as open, resetting the half-open time.
func (c *Circuit[IN, OUT]) reopen() {
	c.openedAt.Store(time.Now().UnixMicro())
}

// close closes the circuit.
func (c *Circuit[IN, OUT]) close() {
	c.openedAt.Store(0)
}

// ObserverForCall returns an [Observer] for the incoming call.
// It is called exactly once per call to [Circuit.Call], before calling the wrapped function.
// If the breaker is open, it returns [ErrCircuitOpen] as an error and a nil [Observer].
// If the breaker is closed, it returns a non-nil [Observer] that will be used to observe the result of the call.
//
// It implements [ObserverFactory], so that the [Circuit] can act as the base for [BreakerMiddleware].
func (c *Circuit[IN, OUT]) ObserverForCall(_ context.Context, state State) (Observer, error) {
	if state == StateOpen {
		return nil, ErrCircuitOpen
	}
	return stateObserver[IN, OUT]{
		circuit: c,
		state:   state,
	}, nil
}

type stateObserver[IN, OUT any] struct {
	circuit *Circuit[IN, OUT]
	state   State
}

func (s stateObserver[IN, OUT]) Observe(failure bool) {
	switch s.circuit.breaker.observe(s.state == StateHalfOpen, failure) {
	case stateChangeNone:
		return // noop
	case stateChangeOpen:
		s.circuit.open()
	case stateChangeClose:
		s.circuit.close()
	}
}

// Call calls the wrapped function if the circuit is closed and returns its result. If the circuit is open, it returns
// [ErrCircuitOpen].
//
// The wrapped function is called synchronously, but possible context errors are recorded as soon as they occur. This
// ensures the circuit opens quickly, even if the wrapped function blocks.
//
// By default, all errors are considered failures (including [context.Canceled]), but this can be customized via
// [WithFailureCondition] and [IgnoreContextCanceled].
//
// Panics are observed as failures, but are not recovered (i.e.: they are "repanicked" instead).
func (c *Circuit[IN, OUT]) Call(ctx context.Context, in IN) (out OUT, err error) {
	if c.f == nil {
		return out, nil
	}

	obs, err := c.observerFactory.ObserverForCall(ctx, c.stateForCall())
	if err != nil {
		// Note: any errors here are not "observed" and do not count towards the breaker's failure rate.
		// This includes:
		// - ErrCircuitOpen
		// - ErrConcurrencyLimit (for blocking limited circuits)
		// - context timeouts while blocked on concurrency limit
		// And any other errors that may be returned by optional breaker wrappers.
		return out, err
	}

	// ensure we dedup the final - potentially wrapped - observer.
	obs = dedupObservableCall(obs)

	obsCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(errWrappedFunctionDone)

	// TODO: we could skip this if we could ensure the original context has neither cancellation nor deadline
	go c.observeCtx(obs, obsCtx)

	defer func() {
		// ensure we also open the breaker on panics
		if err := recover(); err != nil {
			obs.Observe(true)
			panic(err) // let the caller deal with panics
		}
		obs.Observe(c.options.isFailure(err))
	}()

	return c.f(ctx, in)
}

// errWrappedFunctionDone is used to distinguish between internal and external (to the lib) context cancellations.
var errWrappedFunctionDone = errors.New("wrapped function done")

// observeCtx observes the given context for cancellation and records it as a failure.
// It assumes [Observer] is idempotent and deduplicates calls itself.
func (c *Circuit[IN, OUT]) observeCtx(obs Observer, ctx context.Context) {
	// We want to observe a context error as soon as possible to open the breaker, but at the same time we want to
	// keep the call to the wrapped function synchronous to avoid all pitfalls that come with asynchronicity.
	<-ctx.Done()

	err := ctx.Err()
	if context.Cause(ctx) == errWrappedFunctionDone {
		err = nil // ignore internal cancellations; the wrapped function returned already
	}
	obs.Observe(c.options.isFailure(err))
}

// State represents the state of a circuit.
type State int

const (
	// StateClosed means a circuit is ready to accept calls.
	StateClosed State = iota
	// StateHalfOpen means a limited (~1) number of calls is allowed through.
	StateHalfOpen
	// StateOpen means a circuit is not accepting calls.
	StateOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateHalfOpen:
		return "half-open"
	case StateOpen:
		return "open"
	default:
		return "unknown"
	}
}

// defaultFailureCondition is the default failure condition used by [NewCircuit].
// It considers any non-nil error a failure.
func defaultFailureCondition(err error) bool {
	return err != nil
}

type noopBreaker struct{}

func (noopBreaker) observe(halfOpen, failure bool) stateChange {
	return stateChangeNone
}

func (noopBreaker) apply(*options) error {
	return nil
}
