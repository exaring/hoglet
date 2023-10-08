package hoglet

import (
	"math"
	"sync/atomic"
	"time"
)

// state is unexported because its usefulness is unclear outside of tests.
type state int

const (
	stateClosed state = iota
	stateHalfOpen
	stateOpen
)

func (s state) String() string {
	switch s {
	case stateClosed:
		return "closed"
	case stateHalfOpen:
		return "half-open"
	case stateOpen:
		return "open"
	default:
		return "unknown"
	}
}

type Observable interface {
	// Observe is called after the wrapped function returns. If [Circuit.Do] returns a non-nil [Observable], it will be
	// called exactly once.
	Observe(failure bool)
}

// callable encapsulates the common open/half-open/closed logic for all breakers. Each breaker implements decision of
// *when* to open based on its own state.
type callable struct {
	halfOpenDelay time.Duration

	// State

	openedAt atomic.Int64 // unix microseconds
}

// state checks if the callable can be called. If it returns true, the caller MUST then proceed to Call() to ensure
// the half-open logic is respected.
func (c *callable) state() state {
	oa := c.openedAt.Load()

	if oa == 0 {
		// closed
		return stateClosed
	}

	if time.Since(time.UnixMicro(oa)) < c.halfOpenDelay {
		// open
		return stateOpen
	}

	// We reset openedAt to block further calls to pass through when half-open. A success will cause the breaker to
	// close.
	// CompareAndSwap is needed to avoid clobbering another goroutine's openedAt value.
	c.openedAt.CompareAndSwap(oa, time.Now().UnixMicro())
	// half-open
	return stateHalfOpen
}

type observableFunc func(failure bool)

func (o observableFunc) Observe(failure bool) {
	o(failure)
}

// EWMABreaker is a [Breaker] that uses an exponentially weighted moving failure rate between 0 and 1. Each failure
// counting as 1, and each success as 0.
// It assumes the wrapped function is called with an approximately constant interval and will skew results otherwise.
// A zero EWMABreaker is ready to use, but will never open.
//
// Compared to the [SlidingWindowBreaker], this breaker responds faster to failure bursts, but is more lenient with
// constant failure rates.
type EWMABreaker struct {
	decay     float64
	threshold float64

	// State
	failureRate atomic.Value
	callable
}

// NewEWMABreaker creates a new [EWMABreaker] with the given sample count and threshold.
//
// The sample count is used to determine how fast previous observations "decay". A value of 1 causes a single sample to
// be considered. A higher value slows down convergence. As a rule of thumb, breakers with higher throughput should use
// higher sample counts to avoid opening up on small hiccups.
//
// The threshold is the failure rate above which the breaker should open.
//
// The halfOpenDelay is the duration the breaker will stay open before switching to the half-open state, where a
// limited amount of calls are allowed and - if successful - may re-close the breaker.
// Setting it to 0 will cause the breaker to effectively never open.
func NewEWMABreaker(sampleCount int, threshold float64, halfOpenDelay time.Duration) *EWMABreaker {
	e := &EWMABreaker{
		// https://en.wikipedia.org/wiki/Exponential_smoothing
		decay:     2 / (float64(sampleCount)/2 + 1),
		threshold: threshold,
		callable: callable{
			halfOpenDelay: halfOpenDelay,
		},
	}

	e.failureRate.Store(float64(math.SmallestNonzeroFloat64)) // start closed; also work around "initial value" problem

	return e
}

func (e *EWMABreaker) Call() Observable {
	switch e.callable.state() {
	case stateClosed:
		return observableFunc(func(failure bool) {
			e.observe(false, failure)
		})
	case stateHalfOpen:
		return observableFunc(func(failure bool) {
			e.observe(true, failure)
		})
	case stateOpen:
		return nil
	default:
		panic("hoglet: unknown state") // should not happen; see callable.state()
	}
}

func (e *EWMABreaker) observe(halfOpen, failure bool) {
	if e.threshold == 0 {
		return
	}

	if !failure && halfOpen {
		e.openedAt.Store(0)
		e.failureRate.Store(e.threshold)
		return
	}

	var value float64 = 0.0
	if failure {
		value = 1.0
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
		e.openedAt.CompareAndSwap(0, time.Now().UnixMicro())
	} else {
		e.openedAt.Store(0)
	}
}

// SlidingWindowBreaker is a [Breaker] that uses a sliding window to determine the error rate.
type SlidingWindowBreaker struct {
	windowSize time.Duration
	threshold  float64

	// State

	currentStart        atomic.Int64 // in unix microseconds
	currentSuccessCount atomic.Int64
	currentFailureCount atomic.Int64
	lastSuccessCount    atomic.Int64
	lastFailureCount    atomic.Int64

	callable
}

// NewSlidingWindowBreaker creates a new [SlidingWindowBreaker] with the given window size, failure rate threshold and
// half-open delay.
//
// If no observations are made in the window, the breaker will default to closed.
// This means: if halfOpenDelay is bigger than windowSize, the breaker will never enter half-open state and instead
// directly close.
// Conversely, if halfOpenDelay is smaller than windowSize, the errors observed in the last window will still count
// proportionally in half-open state, which will lead to faster re-opening on errors.
func NewSlidingWindowBreaker(windowSize time.Duration, threshold float64, halfOpenDelay time.Duration) *SlidingWindowBreaker {
	s := &SlidingWindowBreaker{
		windowSize: windowSize,
		threshold:  threshold,
		callable: callable{
			halfOpenDelay: halfOpenDelay,
		},
	}

	return s
}

func (s *SlidingWindowBreaker) Call() Observable {
	switch s.callable.state() {
	case stateClosed:
		return observableFunc(func(failure bool) {
			s.observe(false, failure)
		})
	case stateHalfOpen:
		return observableFunc(func(failure bool) {
			s.observe(true, failure)
		})
	case stateOpen:
		return nil
	default:
		panic("hoglet: unknown state") // should not happen; see callable.state()
	}
}

func (s *SlidingWindowBreaker) observe(halfOpen, failure bool) {
	var (
		lastFailureCount    int64
		lastSuccessCount    int64
		currentFailureCount int64
		currentSuccessCount int64
	)

	if !failure && halfOpen {
		s.openedAt.Store(0)
		return
	}

	// The second condition ensures only one goroutine can swap the windows. Necessary since multiple swaps would
	// overwrite the last counts to some near zero value.
	if currentStartMicros := s.currentStart.Load(); sinceMicros(currentStartMicros) > s.windowSize && s.currentStart.CompareAndSwap(currentStartMicros, time.Now().UnixMicro()) {
		lastFailureCount = s.lastFailureCount.Swap(s.currentFailureCount.Swap(0))
		lastSuccessCount = s.lastSuccessCount.Swap(s.currentSuccessCount.Swap(0))
		s.openedAt.Store(0)
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
		s.openedAt.CompareAndSwap(0, time.Now().UnixMicro())
	} else {
		s.openedAt.Store(0)
	}
}

func sinceMicros(micros int64) time.Duration {
	return time.Since(time.UnixMicro(micros))
}
