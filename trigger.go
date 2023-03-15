package hoglet

import (
	"math"
	"sync/atomic"
	"time"
)

// State represents the possible states of a [Trigger].
type State int

const (
	// StateClosed means the [Trigger] is closed and will call the wrapped function.
	StateClosed State = iota
	// StateOpen means the [Trigger] is open and will NOT call the wrapped function.
	StateOpen
	// StateHalfOpen means the [Trigger] is half-open, allowing just a limited number of calls to the wrapped function.
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

type triggerable struct {
	halfOpenDelay time.Duration

	// State
	triggeredAt atomic.Int64 // unix microseconds
}

func (t *triggerable) State() State {
	ta := t.triggeredAt.Load()

	if ta == 0 {
		return StateClosed
	}

	tat := time.UnixMicro(ta)

	if time.Since(tat) < t.halfOpenDelay {
		return StateOpen
	}

	return StateHalfOpen
}

func (t *triggerable) resetOnHalfOpen(failure bool) bool {
	if state := t.State(); state == StateHalfOpen {
		if failure {
			// We reset triggeredAt to block further calls to pass through when half-open. A success will cause the
			// trigger to close.
			t.triggeredAt.Store(time.Now().UnixMicro())
		} else {
			t.triggeredAt.Store(0)
			return true
		}
	}

	return false
}

// EWMATrigger is a [Trigger] that uses an exponentially weighted moving failure rate between 0 and 1. Each failure
// counting as 1, and each success as 0.
// It assumes the wrapped function is called with an approximately constant interval and will skew results otherwise.
// A zero EWMATrigger is ready to use, but will never open.
//
// Compared to the [SlidingWindowTrigger], this trigger responds faster to failure bursts, but is more lenient with
// constant failure rates.
type EWMATrigger struct {
	decay     float64
	threshold float64

	// State
	failureRate atomic.Value
	triggerable
}

// NewEWMATrigger creates a new EWMATrigger with the given sample count and threshold.
//
// The sample count is used to determine how fast previous observations "decay". A value of 1 causes a single sample to
// be considered. A higher value slows down convergence. As a rule of thumb, breakers with higher throughput should use
// higher sample counts to avoid opening up on small hiccups.
//
// The threshold is the failure rate above which the breaker should open.
//
// The halfOpenDelay is the duration the breaker will stay open before switching to the half-open state, where a
// limited amount of calls are allowed and - if successful - may reopen the trigger.
// Setting it to 0 will cause the trigger to effectively never fire.
func NewEWMATrigger(sampleCount int, threshold float64, halfOpenDelay time.Duration) *EWMATrigger {
	e := &EWMATrigger{
		// https://en.wikipedia.org/wiki/Exponential_smoothing
		decay:       2 / (float64(sampleCount)/2 + 1),
		threshold:   threshold,
		triggerable: triggerable{halfOpenDelay: halfOpenDelay},
	}

	e.failureRate.Store(float64(math.SmallestNonzeroFloat64)) // start closed; also work around "initial value" problem

	return e
}

func (e *EWMATrigger) Observe(failure bool) {
	if e.threshold == 0 {
		return
	}

	var value float64 = 0.0
	if failure {
		value = 1.0
	}

	if e.resetOnHalfOpen(failure) {
		e.failureRate.Store(e.threshold)
		return
	}

	// Unconditionally setting via swap and maybe overwriting is faster in the initial case.
	failureRate, _ := e.failureRate.Swap(value).(float64)
	if failureRate == 0 {
		failureRate = value
	} else {
		failureRate = (value * e.decay) + (failureRate * (1 - e.decay))
		e.failureRate.Store(failureRate)
	}

	if failureRate > e.threshold {
		e.triggeredAt.CompareAndSwap(0, time.Now().UnixMicro())
	} else {
		e.triggeredAt.Store(0)
	}
}

// SlidingWindowTrigger is a [Trigger] that uses a sliding window to determine the error rate.
type SlidingWindowTrigger struct {
	windowSize time.Duration
	threshold  float64

	// State
	currentStart        atomic.Int64 // in unix microseconds
	currentSuccessCount atomic.Int64
	currentFailureCount atomic.Int64
	lastSuccessCount    atomic.Int64
	lastFailureCount    atomic.Int64

	triggerable
}

// NewSlidingWindowTrigger creates a new [SlidingWindowTrigger] with the given window size, threshold and half-open
// delay.
//
// If no observations are made in the window, the breaker will default to closed.
// This means: if halfOpenDelay is bigger than windowSize, the breaker will never enter half-open state and instead
// directly close.
// Conversely, if halfOpenDelay is smaller than windowSize, the errors observed in the last window will still count
// proportionally in half-open state, which will lead to faster re-opening on errors.
func NewSlidingWindowTrigger(windowSize time.Duration, threshold float64, halfOpenDelay time.Duration) *SlidingWindowTrigger {
	s := &SlidingWindowTrigger{
		windowSize:  windowSize,
		threshold:   threshold,
		triggerable: triggerable{halfOpenDelay: halfOpenDelay},
	}

	return s
}

func (s *SlidingWindowTrigger) Observe(failure bool) {
	var (
		lastFailureCount    int64
		lastSuccessCount    int64
		currentFailureCount int64
		currentSuccessCount int64
	)

	if s.resetOnHalfOpen(failure) {
		return
	}

	// The second condition ensures only one goroutine can swap the windows. Necessary since multiple swaps would
	// overwrite the last counts to some near zero value.
	if currentStartMicros := s.currentStart.Load(); sinceMicros(currentStartMicros) > s.windowSize && s.currentStart.CompareAndSwap(currentStartMicros, time.Now().UnixMicro()) {
		lastFailureCount = s.lastFailureCount.Swap(s.currentFailureCount.Swap(0))
		lastSuccessCount = s.lastSuccessCount.Swap(s.currentSuccessCount.Swap(0))
		s.triggeredAt.Store(0)
	} else {
		lastFailureCount = s.lastFailureCount.Load()
		lastSuccessCount = s.lastSuccessCount.Load()
	}

	if failure {
		currentFailureCount = s.currentFailureCount.Add(1)
		currentSuccessCount = s.currentSuccessCount.Load()
	} else {
		currentSuccessCount = s.currentSuccessCount.Add(1)
		currentFailureCount = s.currentFailureCount.Load()
	}

	// We use the last window's weight to determine how much the last window's failure rate should count.
	// It is the remaining portion of the last window still "visible" in the current window.
	lastWindowWeight := (sinceMicros(s.currentStart.Load()).Seconds() - s.windowSize.Seconds()) / s.windowSize.Seconds()

	weightedFailures := float64(lastFailureCount)*lastWindowWeight + float64(currentFailureCount)
	weightedTotal := float64(lastFailureCount+lastSuccessCount)*lastWindowWeight + float64(currentFailureCount+currentSuccessCount)
	failureRate := weightedFailures / weightedTotal

	if failureRate > s.threshold {
		s.triggeredAt.CompareAndSwap(0, time.Now().UnixMicro())
	} else {
		s.triggeredAt.Store(0)
	}
}

func sinceMicros(micros int64) time.Duration {
	return time.Since(time.UnixMicro(micros))
}
