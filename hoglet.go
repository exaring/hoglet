package hoglet

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// Circuit wraps a function and behaves like a simple circuit and breaker: it opens when the wrapped function fails and
// stops calling the wrapped function until it closes again, returning [ErrCircuitOpen] in the meantime.
//
// A zero circuit will not panic, but wraps a noop. Use [NewCircuit] instead.
type Circuit[IN, OUT any] struct {
	f       BreakableFunc[IN, OUT]
	breaker Breaker
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
}

// Breaker is the interface implemented by the different breakers, responsible for actually opening the circuit.
// Each implementation behaves differently when deciding whether to open the breaker upon failure.
type Breaker interface {
	// connect is called to allow the breaker to actuate its parent circuit.
	connect(untypedCircuit)

	// observerForCall returns an observer for the incoming call.
	// It is called exactly once per call to [Breaker.Do], before calling the wrapped function.
	// If the breaker is open, it returns nil.
	// If the breaker is closed, it returns a non-nil [Observable] that will be used to observe the result of the call.
	observerForCall() observer
}

// BreakableFunc is the type of the function wrapped by a Breaker.
type BreakableFunc[IN, OUT any] func(context.Context, IN) (OUT, error)

// NewCircuit instantiates a new circuit breaker that wraps the given function. See [Circuit.Call] for calling semantics.
// A Circuit with a nil breaker will never open.
func NewCircuit[IN, OUT any](f BreakableFunc[IN, OUT], breaker Breaker, opts ...Option) *Circuit[IN, OUT] {
	b := &Circuit[IN, OUT]{
		f:       f,
		breaker: breaker,
		options: options{
			isFailure: defaultFailureCondition,
		},
	}

	if breaker != nil {
		breaker.connect(b)
	}

	for _, opt := range opts {
		opt.apply(&b.options)
	}

	return b
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
		// CompareAndSwap is needed to avoid clobbering another goroutine's openedAt value.
		c.setOpenedAt(time.Now().UnixMicro())
	}

	return state
}

// SetOpenedAt sets the time the circuit was opened at.
// Passing a value of 0 (re)opens the cirtuit.
func (c *Circuit[IN, OUT]) setOpenedAt(i int64) {
	if i == 0 {
		c.openedAt.Store(i)
	} else {
		c.openedAt.CompareAndSwap(0, i)
	}
}

// Call calls the wrapped function if the circuit is closed and returns its result. If the circuit is open, it returns
// [ErrCircuitOpen].
//
// The wrapped function is called synchronously, but possilble context errors are recorded as soon as they occur. This
// ensures the circuit opens quickly, even if the wrapped function blocks.
//
// By default, all errors are considered failures (including [context.Canceled]), but this can be customized via
// [WithFailureCondition] and [IgnoreContextCancelation].
//
// Panics are observed as failures, but are not recovered (i.e.: they are "repanicked" instead).
func (c *Circuit[IN, OUT]) Call(ctx context.Context, in IN) (out OUT, err error) {
	if c.f == nil {
		return out, nil
	}

	obs := c.observerForCall()
	if obs == nil {
		return out, ErrCircuitOpen
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(internalCancellation)

	// TODO: we could skip this if we could ensure the original context has neither cancellation nor deadline
	go c.observeCtx(obs, ctx)

	defer func() {
		// ensure we also open the breaker on panics
		if err := recover(); err != nil {
			obs.observe(true)
			panic(err) // let the caller deal with panics
		}
		obs.observe(c.options.isFailure(err))
	}()

	return c.f(ctx, in)
}

// observerForCall is a thin wrapper around the breaker to simplify the case where no breaker has been set.
func (c *Circuit[IN, OUT]) observerForCall() observer {
	if c.breaker == nil {
		return noopObserveable{}
	}
	return c.breaker.observerForCall()
}

// internalCancellation is used to distinguish between internal and external (to the lib) context cancellations.
var internalCancellation = errors.New("internal cancellation")

// observeCtx observes the given context for cancellation and records it as a failure.
// It assumes [Observable.Observe] is idempotent and deduplicates calls itself.
func (c *Circuit[IN, OUT]) observeCtx(obs observer, ctx context.Context) {
	// We want to observe a context error as soon as possible to open the breaker, but at the same time we want to
	// keep the call to the wrapped function synchronous to avoid all pitfalls that come with asynchronicity.
	<-ctx.Done()

	err := ctx.Err()
	if context.Cause(ctx) == internalCancellation {
		err = nil // ignore internal cancellations; the wrapped function returned already
	}
	obs.observe(c.options.isFailure(err))
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
// It consider any non-nil error a failure.
func defaultFailureCondition(err error) bool {
	return err != nil
}

type noopObserveable struct{}

func (noopObserveable) observe(bool) {}
