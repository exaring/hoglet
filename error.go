package hoglet

import "errors"

var (
	// ErrCircuitOpen is returned when a circuit is open and not allowing calls through.
	ErrCircuitOpen = errors.New("hoglet: breaker is open")
	// ErrConcurrencyLimitReached is returned by a [Circuit] using [ConcurrencyLimiter] in non-blocking mode when the
	// set limit is reached.
	ErrConcurrencyLimitReached = errors.New("hoglet: concurrency limit reached")
	// ErrWaitingForSlot is returned by a [Circuit] using [ConcurrencyLimiter] in blocking mode when a context error
	// occurs while waiting for a slot.
	ErrWaitingForSlot = errors.New("hoglet: waiting for slot")
)
