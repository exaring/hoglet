package hoglet

// Error is the error type used for circuit breaker errors. It can be used to separate circuit errors from errors
// returned by the wrapped function.
type Error struct {
	msg string
}

// Error implements the error interface.
func (b Error) Error() string {
	return "hoglet: " + b.msg
}

var (
	// ErrCircuitOpen is returned when a circuit is open and not allowing calls through.
	ErrCircuitOpen = Error{msg: "breaker is open"}
	// ErrConcurrencyLimitReached is returned by a [Circuit] using [WithConcurrencyLimit] in non-blocking mode when the
	// set limit is reached.
	ErrConcurrencyLimitReached = Error{msg: "concurrency limit reached"}
	// ErrWaitingForSlot is returned by a [Circuit] using [WithConcurrencyLimit] in blocking mode when a context error
	// occurs while waiting for a slot.
	ErrWaitingForSlot = Error{msg: "waiting for slot"}
)
