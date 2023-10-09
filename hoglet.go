package hoglet

import (
	"context"
	"errors"
)

// Circuit wraps a function and behaves like a simple circuit and breaker: it opens when the wrapped function fails and
// stops calling the wrapped function until it closes again, returning [ErrCircuitOpen] in the meantime.
//
// A zero circuit will not panic, but wraps a noop. Use [NewBreaker] instead.
type Circuit[IN, OUT any] struct {
	f       BreakableFunc[IN, OUT]
	breaker Breaker
	options options
}

// options is a sub-struct to avoid requiring type parameters in the [Option] type.
type options struct {
	// isFailure is a filter function that determines whether an error can open the breaker.
	isFailure func(error) bool
}

// Breaker is the interface implemented by the different breakers, responsible for actually opening the circuit.
// Each implementation behaves differently when deciding whether to open the breaker upon failure.
type Breaker interface {
	// Call attempts to call the wrapped function.
	// It is called exactly once per call to [Breaker.Do], before calling the wrapped function.
	// If the breaker is open, it returns nil.
	// If the breaker is closed, it returns a non-nil [Observable] that will be used to observe the result of the call.
	Call() Observable
}

// BreakableFunc is the type of the function wrapped by a Breaker.
type BreakableFunc[IN, OUT any] func(context.Context, IN) (OUT, error)

// NewCircuit instantiates a new breaker that wraps the given function. See [Do] for calling semantics.
// A Circuit with a nil breaker will never open.
func NewCircuit[IN, OUT any](f BreakableFunc[IN, OUT], breaker Breaker, opts ...Option) *Circuit[IN, OUT] {
	b := &Circuit[IN, OUT]{
		f:       f,
		breaker: breaker,
		options: options{
			isFailure: defaultFailureCondition,
		},
	}

	for _, opt := range opts {
		opt.apply(&b.options)
	}

	return b
}

// Do calls the wrapped function and returns its result. If the circuit is open, it returns [ErrCircuitOpen].
//
// The wrapped function is called synchronously, but possilble context errors are recorded as soon as they occur. This
// ensures the circuit opens quickly, even if the wrapped function blocks.
//
// By default, all errors are considered failures (including [context.Canceled]), but this can be customized via
// [WithFailureCondition] and [IgnoreContextCancelation].
//
// Panics are observed as failures, but are not recovered ("repanicked").
func (b *Circuit[IN, OUT]) Do(ctx context.Context, in IN) (out OUT, err error) {
	if b.f == nil {
		return out, nil
	}

	obs := b.observable()
	if obs == nil {
		return out, ErrCircuitOpen
	}

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(internalCancellation)
	go b.observeCtx(obs, ctx)

	defer func() {
		// ensure we also open the breaker on panics
		if err := recover(); err != nil {
			obs.Observe(true)
			panic(err) // let the caller deal with panics
		}
		obs.Observe(b.options.isFailure(err))
	}()

	return b.f(ctx, in)
}

func (b *Circuit[IN, OUT]) observable() Observable {
	if b.breaker == nil {
		return noopObserveable{}
	}
	return b.breaker.Call()
}

// internalCancellation is used to distinguish between internal and external (to the lib) context cancellations.
var internalCancellation = errors.New("internal cancellation")

// observeCtx observes the given context for cancellation and records it as a failure.
// It assumes [Observable.Observe] is idempotent and deduplicates calls itself.
func (b *Circuit[IN, OUT]) observeCtx(obs Observable, ctx context.Context) {
	// We want to observe a context error as soon as possible to open the breaker, but at the same time we want to
	// keep the call to the wrapped function synchronous to avoid all pitfalls that come with asynchronicity.
	<-ctx.Done()

	err := ctx.Err()
	if context.Cause(ctx) == internalCancellation {
		err = nil // ignore internal cancellations; the wrapped function returned already
	}
	obs.Observe(b.options.isFailure(err))
}

// defaultFailureCondition is the default failure condition used by [NewCircuit].
// It consider any non-nil error a failure.
func defaultFailureCondition(err error) bool {
	return err != nil
}

type noopObserveable struct{}

func (noopObserveable) Observe(bool) {}
