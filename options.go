package hoglet

import "context"

type Option interface {
	apply(*options)
}

type optionFunc func(*options)

func (f optionFunc) apply(o *options) {
	f(o)
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
