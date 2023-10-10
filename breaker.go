package hoglet

import (
	"context"
	"math"
	"sync/atomic"
	"time"
)

// untypedCircuit is used to avoid type annotations when implementing a breaker.
type untypedCircuit interface {
	stateForCall() State
	setOpenedAt(int64)
}

type observer interface {
	// observe is called after the wrapped function returns. If [Circuit.Do] returns a non-nil [Observable], it will be
	// called exactly once.
	observe(failure bool)
}

// observableCall tracks a single call through the breaker.
// It should be instantiated via [newObservableCall] to ensure the observer is only called once.
type observableCall func(bool)

func (o observableCall) observe(failure bool) {
	o(failure)
}

// EWMABreaker is a [Breaker] that uses an exponentially weighted moving failure rate. See [NewEWMABreaker] for details.
//
// A zero EWMABreaker is ready to use, but will never open.
type EWMABreaker struct {
	decay     float64
	threshold float64
	circuit   untypedCircuit

	// State
	failureRate atomic.Value
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
// The sample count is used to determine how fast previous observations "decay". A value of 1 causes a single sample to
// be considered. A higher value slows down convergence. As a rule of thumb, breakers with higher throughput should use
// higher sample counts to avoid opening up on small hiccups.
//
// The failureThreshold is the failure rate above which the breaker should open (0.0-1.0).
func NewEWMABreaker(sampleCount uint, failureThreshold float64) *EWMABreaker {
	e := &EWMABreaker{
		// https://en.wikipedia.org/wiki/Exponential_smoothing
		decay:     2 / (float64(sampleCount)/2 + 1),
		threshold: failureThreshold,
	}

	e.failureRate.Store(float64(math.SmallestNonzeroFloat64)) // start closed; also work around "initial value" problem

	return e
}

func (e *EWMABreaker) connect(c untypedCircuit) {
	e.circuit = c
}

func (e *EWMABreaker) observerForCall(_ context.Context) (observer, error) {
	state := e.circuit.stateForCall()

	if state == StateOpen {
		return nil, ErrCircuitOpen
	}

	return observableCall(func(failure bool) {
		e.observe(state == StateHalfOpen, failure)
	}), nil
}

func (e *EWMABreaker) observe(halfOpen, failure bool) {
	if e.threshold == 0 {
		return
	}

	if !failure && halfOpen {
		e.circuit.setOpenedAt(0)
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
		e.circuit.setOpenedAt(time.Now().UnixMicro())
	} else {
		e.circuit.setOpenedAt(0)
	}
}

// SlidingWindowBreaker is a [Breaker] that uses a sliding window to determine the error rate.
type SlidingWindowBreaker struct {
	windowSize time.Duration
	threshold  float64
	circuit    untypedCircuit

	// State

	currentStart        atomic.Int64 // in unix microseconds
	currentSuccessCount atomic.Int64
	currentFailureCount atomic.Int64
	lastSuccessCount    atomic.Int64
	lastFailureCount    atomic.Int64
}

// NewSlidingWindowBreaker creates a new [SlidingWindowBreaker] with the given window size and failure rate threshold.
//
// This is a time-based breaker, which means it will revert back to closed after its window size has passed: if no
// observations are made in the window, the failure rate is effectively zero.
// This also means: if the circuit has a halfOpenDelay and it is bigger than windowSize, the breaker will never enter
// half-open state and will directly close instead.
// Conversely, if halfOpenDelay is smaller than windowSize, the errors observed in the last window will still count
// proportionally in half-open state, which will lead to faster re-opening on errors.
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

func (s *SlidingWindowBreaker) connect(c untypedCircuit) {
	s.circuit = c
}

func (s *SlidingWindowBreaker) observerForCall(_ context.Context) (observer, error) {
	state := s.circuit.stateForCall()

	if state == StateOpen {
		return nil, ErrCircuitOpen
	}

	return observableCall(func(failure bool) {
		s.observe(state == StateHalfOpen, failure)
	}), nil
}

func (s *SlidingWindowBreaker) observe(halfOpen, failure bool) {
	var (
		lastFailureCount    int64
		lastSuccessCount    int64
		currentFailureCount int64
		currentSuccessCount int64
	)

	if !failure && halfOpen {
		s.circuit.setOpenedAt(0)
		return
	}

	// The second condition ensures only one goroutine can swap the windows. Necessary since multiple swaps would
	// overwrite the last counts to some near zero value.
	if currentStartMicros := s.currentStart.Load(); sinceMicros(currentStartMicros) > s.windowSize && s.currentStart.CompareAndSwap(currentStartMicros, time.Now().UnixMicro()) {
		lastFailureCount = s.lastFailureCount.Swap(s.currentFailureCount.Swap(0))
		lastSuccessCount = s.lastSuccessCount.Swap(s.currentSuccessCount.Swap(0))
		s.circuit.setOpenedAt(0)
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
		s.circuit.setOpenedAt(time.Now().UnixMicro())
	} else {
		s.circuit.setOpenedAt(0)
	}
}

func sinceMicros(micros int64) time.Duration {
	return time.Since(time.UnixMicro(micros))
}
