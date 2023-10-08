package hoglet

type Error struct {
	msg string
}

func (b Error) Error() string {
	return "hoglet: " + b.msg
}

var ErrCircuitOpen = Error{msg: "breaker is open"}
