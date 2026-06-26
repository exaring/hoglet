package hoglet

import (
	"fmt"
	"math"
	"sync/atomic"
	"time"
)

// stateChange encodes what the circuit should do after observing a call.
type stateChange int

const (
	// stateChangeNone means the circuit should keep its current state.
	stateChangeNone stateChange = iota
	// stateChangeOpen means the circuit should open.
	stateChangeOpen
	// stateChangeClose means the circuit should close.
	stateChangeClose
)

func (s stateChange) String() string {
	switch s {
	case stateChangeNone:
		return "none"
	case stateChangeOpen:
		return "open"
	case stateChangeClose:
		return "close"
	default:
		return "unknown"
	}
}

// Observer is used to observe the result of a single wrapped call through the circuit breaker.
// Calls in an open circuit cause no observer to be created.
type Observer interface {
	// Observe is called after the wrapped function returns. If [ObserverForCall] returns a non-nil [Observer], it will be
	// called exactly once.
	Observe(failure bool)
}

// ObserverFunc is a helper to turn any function into an [Observer].
type ObserverFunc func(bool)

func (o ObserverFunc) Observe(failure bool) {
	o(failure)
}

func fromStore(i uint64) float64 {
	return math.Float64frombits(i)
}

func toStore(i float64) uint64 {
	return math.Float64bits(i)
}

// EWMABreaker is a [Breaker] that uses an exponentially weighted moving failure rate. See [NewEWMABreaker] for details.
//
// A zero EWMABreaker is ready to use, but will never open.
type EWMABreaker struct {
	decay     float64
	threshold float64

	// State
	failureRate atomic.Uint64
}

// NewEWMABreaker creates a new [EWMABreaker] with the given sample count and threshold. It uses an Exponentially
// Weighted Moving Average to calculate the current failure rate.
//
// ⚠️ This is an observation-based breaker, which means it requires new calls to be able to update the failure rate, and
// therefore REQUIRES the circuit to set a half-open threshold via [WithHalfOpenDelay]. Otherwise an open circuit will
// never observe any successes and thus never close.
//
// Compared to the [SlidingWindowBreaker], this breaker responds faster to failure bursts, but is more lenient with
// constant failure rates.
//
// The sample count must be at least 1 and is used to determine how fast previous observations "decay". A value of 1
// causes only the newest sample to be considered. A higher value slows down convergence. As a rule of thumb, breakers
// with higher throughput should use higher sample counts to avoid opening up on small hiccups. A sample count of 0 is
// rejected by [NewCircuit].
//
// The failureThreshold is the failure rate above which the breaker should open (0.0-1.0).
func NewEWMABreaker(sampleCount uint, failureThreshold float64) *EWMABreaker {
	e := &EWMABreaker{
		// Span-based exponential smoothing factor: https://en.wikipedia.org/wiki/Exponential_smoothing
		// decay = 2/(N+1) lies in (0,1] for every sampleCount >= 1 (and is exactly 1 at sampleCount == 1, i.e. only the
		// newest sample counts). sampleCount == 0 yields decay = 2, which is rejected in apply.
		decay:     2 / (float64(sampleCount) + 1),
		threshold: failureThreshold,
	}

	e.failureRate.Store(toStore(math.SmallestNonzeroFloat64)) // start closed; also work around "initial value" problem

	return e
}

func (e *EWMABreaker) observe(halfOpen, failure bool) stateChange {
	if e.threshold == 0 {
		return stateChangeNone
	}

	if !failure && halfOpen {
		e.failureRate.Store(toStore(e.threshold))
		return stateChangeClose
	}

	var value = 0.0
	if failure {
		value = 1.0
	}

	// Unconditionally setting via swap and maybe overwriting is faster in the initial case.
	failureRate := fromStore(e.failureRate.Swap(toStore(value)))
	if failureRate == math.SmallestNonzeroFloat64 {
		failureRate = value
	} else {
		failureRate = (value * e.decay) + (failureRate * (1 - e.decay))
		e.failureRate.Store(toStore(failureRate))
	}

	if failureRate > e.threshold {
		return stateChangeOpen
	} else {
		return stateChangeClose
	}
}

// apply implements Option.
func (e *EWMABreaker) apply(o *options) error {
	if o.halfOpenDelay == 0 {
		return fmt.Errorf("EWMABreaker requires a half-open delay")
	}

	if e.threshold < 0 || e.threshold > 1 {
		return fmt.Errorf("EWMABreaker threshold must be between 0 and 1")
	}

	// decay > 1 means an out-of-range smoothing factor, which only happens for a sample count of 0 (see
	// [NewEWMABreaker]). The zero-value breaker has decay == 0 and is deliberately allowed (it never opens).
	if e.decay > 1 {
		return fmt.Errorf("EWMABreaker requires a sample count of at least 1")
	}

	return nil
}

// SlidingWindowBreaker is a [Breaker] that uses a sliding window to determine the error rate.
type SlidingWindowBreaker struct {
	windowSize time.Duration
	threshold  float64

	// State

	currentStart        atomic.Int64 // monotonic nanoseconds since start (see nowNanos)
	currentSuccessCount atomic.Int64
	currentFailureCount atomic.Int64
	lastSuccessCount    atomic.Int64
	lastFailureCount    atomic.Int64
}

// NewSlidingWindowBreaker creates a new [SlidingWindowBreaker] with the given window size and failure rate threshold.
//
// This is a time-based breaker, which means it will revert back to closed after its window size has passed: if no
// observations are made in the window, the failure rate is effectively zero.
// The half-open delay (see [WithHalfOpenDelay]) defaults to windowSize when unset and may not exceed it (a larger delay
// would never let the circuit go half-open, since the window expires and closes it first); an explicit larger value is
// rejected by [NewCircuit].
// If halfOpenDelay is smaller than windowSize, the errors observed in the last window will still count proportionally in
// half-open state, which will lead to faster re-opening on errors.
//
// The windowSize is the time interval over which to calculate the failure rate.
//
// The failureThreshold is the failure rate above which the breaker should open (0.0-1.0).
func NewSlidingWindowBreaker(windowSize time.Duration, failureThreshold float64) *SlidingWindowBreaker {
	s := &SlidingWindowBreaker{
		windowSize: windowSize,
		threshold:  failureThreshold,
	}

	return s
}

func (s *SlidingWindowBreaker) observe(halfOpen, failure bool) stateChange {
	var (
		lastFailureCount    int64
		lastSuccessCount    int64
		currentFailureCount int64
		currentSuccessCount int64
	)

	if !failure && halfOpen {
		return stateChangeClose
	}

	currentStartNanos := s.currentStart.Load()
	sinceStart := sinceNanos(currentStartNanos)

	// Rotate the windows once the current one has passed (or initialize it on the very first observation). The
	// CompareAndSwap ensures only one goroutine swaps the windows; multiple swaps would overwrite the last counts to
	// some near zero value.
	if (currentStartNanos == 0 || sinceStart > s.windowSize) && s.currentStart.CompareAndSwap(currentStartNanos, nowNanos()) {
		sinceStart = 0
		lastFailureCount = s.lastFailureCount.Swap(s.currentFailureCount.Swap(0))
		lastSuccessCount = s.lastSuccessCount.Swap(s.currentSuccessCount.Swap(0))
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
	lastWindowWeight := max(0, s.windowSize.Seconds()-sinceStart.Seconds()) / s.windowSize.Seconds()

	weightedFailures := float64(lastFailureCount)*lastWindowWeight + float64(currentFailureCount)
	weightedTotal := float64(lastFailureCount+lastSuccessCount)*lastWindowWeight + float64(currentFailureCount+currentSuccessCount)
	failureRate := weightedFailures / weightedTotal

	if failureRate > s.threshold {
		return stateChangeOpen
	} else {
		return stateChangeClose
	}
}

// apply implements Option.
func (s *SlidingWindowBreaker) apply(o *options) error {
	if s.threshold < 0 || s.threshold > 1 {
		return fmt.Errorf("SlidingWindowBreaker threshold must be between 0 and 1")
	}

	switch {
	case o.halfOpenDelay == 0:
		// Unset: default to the window size, after which the breaker self-heals anyway.
		o.halfOpenDelay = s.windowSize
	case o.halfOpenDelay > s.windowSize:
		// An explicit delay larger than the window would never let the circuit go half-open (the window expires and
		// closes it first), so reject it instead of silently discarding the caller's value.
		return fmt.Errorf("SlidingWindowBreaker half-open delay (%s) cannot exceed window size (%s)", o.halfOpenDelay, s.windowSize)
	}

	return nil
}
