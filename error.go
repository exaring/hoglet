package hoglet

type BreakerError struct {
	msg string
}

func (b BreakerError) Error() string {
	return "hoglet: " + b.msg
}

var ErrBreakerOpen = BreakerError{msg: "breaker is open"}
