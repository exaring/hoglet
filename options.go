package hoglet

import (
	"context"
	"time"
)

type Option interface {
	apply(*options)
}

type optionFunc func(*options)

func (f optionFunc) apply(o *options) {
	f(o)
}

// WithHalfOpenDelay sets the duration the circuit will stay open before switching to the half-open state, where a
// limited (~1) amount of calls are allowed that - if successful - may re-close the breaker.
func WithHalfOpenDelay(delay time.Duration) Option {
	return optionFunc(func(b *options) {
		b.halfOpenDelay = delay
	})
}

// WithFailureCondition allows specifying a filter function that determines whether an error should open the breaker.
// If the provided function returns true, the error is considered a failure and the breaker may open (depending on the
// breaker logic).
// The default filter considers all non-nil errors as failures (err != nil).
func WithFailureCondition(condition func(error) bool) Option {
	return optionFunc(func(b *options) {
		b.isFailure = condition
	})
}

// IgnoreContextCancelation is a helper function for [WithFailureCondition] that ignores [context.Canceled] errors.
func IgnoreContextCancelation(err error) bool {
	return err != nil && err != context.Canceled
}

// WithConcurrencyLimit sets the maximum number of concurrent calls to the provided limit. If the limit is reached, the
// circuit's behavior depends on the blocking parameter:
//   - it either returns [ErrConcurrencyLimitReached] immediately if blocking is false
//   - or blocks until a slot is available if blocking is true, potentially returning [ErrWaitingForSlot] if the context
//     is canceled or times out while waiting.
func WithConcurrencyLimit(limit int64, blocking bool) Option {
	return optionFunc(func(b *options) {
		b.observerForCall = newLimiter(b.observerForCall, limit, blocking)
	})
}
