package hoglet

import (
	"sync/atomic"
	"time"
)

// used to mock time
var timeNow = time.Now

type State int

const (
	StateOpen State = iota
	StateClosed
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateOpen:
		return "open"
	case StateClosed:
		return "closed"
	case StateHalfOpen:
		return "half-open"
	default:
		return "unknown"
	}
}

// EWMATrigger is a [Trigger] that uses an exponentially weighted moving success rate between 0 and 1. Each success
// counts as 1, and each failure counts as 0.
// It assumes the wrapped function is called with an approximately constant interval and will skew results otherwise.
// A zero EWMATrigger is ready to use, but will never open.
type EWMATrigger struct {
	decay         float64
	threshold     float64
	halfOpenDelay time.Duration

	// State
	successRate atomic.Value
	triggeredAt atomic.Value
}

// NewEWMATrigger creates a new EWMATrigger with the given sample count and threshold.
//
// The sample count is used to determine how fast previous observations "decay". A value of 1 causes a single sample to
// be considered. A higher value slows down convergence. As a rule of thumb, breakers with higher throughput should use
// higher sample counts to avoid opening up on small hiccups.
//
// The threshold is the success rate below which the breaker will open.
//
// The halfOpenDelay is the duration the breaker will stay open before switching to the half-open state, where a
// limited amount of calls are allowed and - if successful - may reopen the trigger.
// Setting it to 0 will cause the trigger to effectively never fire.
func NewEWMATrigger(sampleCount int, threshold float64, halfOpenDelay time.Duration) *EWMATrigger {
	e := &EWMATrigger{
		// https://en.wikipedia.org/wiki/Exponential_smoothing
		decay:         2 / (float64(sampleCount)/2 + 1),
		threshold:     threshold,
		halfOpenDelay: halfOpenDelay,
	}

	e.successRate.Store(float64(1.0)) // start closed
	e.triggeredAt.Store(time.Time{})  // start closed

	return e
}

func (e *EWMATrigger) Observe(failure bool) {
	var value float64 = 1.0
	if failure {
		value = 0.0
	}

	state := e.State()
	if state == StateHalfOpen {
		if failure {
			// We reset triggeredAt to block further calls to pass through when half-open. A success will cause the trigger
			// to close.
			e.triggeredAt.Store(timeNow())
		} else {
			e.triggeredAt.Store(time.Time{})
			e.successRate.Store(e.threshold)
			return
		}
	}

	// Unconditionally setting via swap and maybe overrwriting is faster in the initial case.
	current, _ := e.successRate.Swap(value).(float64)
	if current != 0 {
		current = (value * e.decay) + (current * (1 - e.decay))
		e.successRate.Store(current)
	}

	if current < e.threshold {
		e.triggeredAt.CompareAndSwap(time.Time{}, timeNow())
	} else {
		e.triggeredAt.Store(time.Time{})
	}
}

func (e *EWMATrigger) State() State {
	t, _ := e.triggeredAt.Load().(time.Time)

	if t.IsZero() {
		return StateClosed
	}

	if timeNow().Sub(t) < e.halfOpenDelay {
		return StateOpen
	}

	return StateHalfOpen
}
