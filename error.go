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

var ErrCircuitOpen = Error{msg: "breaker is open"}
