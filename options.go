package hoglet

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Option interface {
	apply(*options) error
}

type optionFunc func(*options) error

func (f optionFunc) apply(o *options) error {
	return f(o)
}

// WithHalfOpenDelay sets the duration the circuit will stay open before switching to the half-open state, where a
// limited (~1) amount of calls are allowed that - if successful - may re-close the breaker.
func WithHalfOpenDelay(delay time.Duration) Option {
	return optionFunc(func(o *options) error {
		o.halfOpenDelay = delay
		return nil
	})
}

// WithFailureCondition allows specifying a filter function that determines whether an error should open the breaker.
// If the provided function returns true, the error is considered a failure and the breaker may open (depending on the
// breaker logic).
// Nil errors are always considered successes. The provided function is only called in the non-nil error case.
//
// This does not modify the error returned by the wrapped function call. It only affects the circuit itself.
func WithFailureCondition(condition func(error) bool) Option {
	return optionFunc(func(o *options) error {
		o.isFailure = condition
		return nil
	})
}

// IgnoreContextCanceled is a helper function for [WithFailureCondition] that ignores [context.Canceled] errors.
func IgnoreContextCanceled(err error) bool {
	return !errors.Is(err, context.Canceled)
}

// WithBreakerMiddleware allows wrapping the [Breaker] via a [BreakerMiddleware].
// Middlewares are processed from innermost to outermost, meaning the first added middleware is the closest to the
// wrapped function.
// ⚠️ This means ordering is significant: since "outer" middleware may react differently depending on the output of
// "inner" middleware. E.g.: the optional prometheus middleware can report metrics about the [ConcurrencyLimiter]
// middleware and should therefore be AFTER it in the parameter list.
func WithBreakerMiddleware(bm BreakerMiddleware) Option {
	return optionFunc(func(o *options) error {
		b, err := bm.Wrap(o.observerFactory)
		if err != nil {
			return fmt.Errorf("creating middleware: %w", err)
		}
		o.observerFactory = b
		return nil
	})
}
