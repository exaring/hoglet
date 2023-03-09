package hoglet

import (
	"context"
	"errors"
	"sync"
)

// Breaker wraps a function and behaves like a circuit breaker: it triggers (opens) when the wrapped function fails and
// stops calling the wrapped function until it closes again, returning [ErrBreakerOpen] in the meantime.
//
// A zero breaker will not panic, but wraps a noop. Use [NewBreaker] instead.
type Breaker[IN, OUT any] struct {
	f       BreakableFunc[IN, OUT]
	trigger Trigger
	options options
}

// We use a sub-struct to avoid requiring type parameters in [Option].
type options struct {
	// isFailure is a filter function that determines whether an error can open the breaker.
	isFailure func(error) bool
}

// Trigger is the interface implemented by the different
type Trigger interface {
	// State returns the current state of the breaker.
	State() State
	// Observe will only be called if [State] returns StateOpen or StateHalfOpen.
	Observe(failure bool)
}

type BreakableFunc[IN, OUT any] func(context.Context, IN) (OUT, error)

// NewBreaker instantiates a new breaker that wraps the given function. See [Do] for calling semantics.
// A Breaker with a nil trigger will never open.
func NewBreaker[IN, OUT any](f BreakableFunc[IN, OUT], tripper Trigger, opts ...Option) *Breaker[IN, OUT] {
	b := &Breaker[IN, OUT]{
		f:       f,
		trigger: tripper,
		options: options{
			isFailure: defaultFailureCondition,
		},
	}

	for _, opt := range opts {
		opt.apply(&b.options)
	}

	return b
}

// Do calls the wrapped function and returns its result. If the breaker is open, it returns [ErrBreakerOpen].
//
// The wrapped function is called synchronously, but possilble context errors are recorded as soon as they occur. This
// ensures the breaker opens quickly, even if the wrapped function blocks.
//
// By default, all errors are considered failures (including [context.Canceled]), but this can be customized via
// [WithFailureCondition] and [IgnoreContextCancelation].
//
// Panics are observed as failures, but are not recovered.
func (b *Breaker[IN, OUT]) Do(ctx context.Context, in IN) (out OUT, err error) {
	if b.f == nil {
		return out, nil
	}

	if b.isOpen() {
		return out, ErrBreakerOpen
	}

	once := &sync.Once{}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(internalCancellation)
	go b.observeCtxOnce(once, ctx)

	defer func() {
		// ensure we also open the circuit on panics
		if err := recover(); err != nil {
			b.observeOnce(once, true)
			panic(err) // let the caller deal with panics
		}
		b.observeOnce(once, b.options.isFailure(err))
	}()

	return b.f(ctx, in)
}

func (b *Breaker[IN, OUT]) isOpen() bool {
	if b.trigger == nil {
		return false
	}
	return b.trigger.State() == StateOpen
}

// Helper to work around the inherent racyness of the "context watch" goroutine and ensure we only observe one result.
func (b *Breaker[IN, OUT]) observeOnce(once *sync.Once, failure bool) {
	once.Do(func() {
		if b.trigger == nil {
			return
		}

		b.trigger.Observe(failure)
	})
}

// internalCancellation is used to distinguish between internal and external (to the lib) context cancellations.
var internalCancellation = errors.New("internal cancellation")

func (b *Breaker[IN, OUT]) observeCtxOnce(once *sync.Once, ctx context.Context) {
	// We want to observe a context error as soon as possible to open the breaker, but at the same time we want to
	// keep the call to the wrapped function synchronous to avoid all pitfalls that come with asynchronicity.
	<-ctx.Done()

	err := ctx.Err()
	if context.Cause(ctx) == internalCancellation {
		err = nil // ignore internal cancellations; the wrapped function returned already
	}
	b.observeOnce(once, b.options.isFailure(err))
}

func defaultFailureCondition(err error) bool {
	return err != nil
}
