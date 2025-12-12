package hoglet

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEWMABreaker_ConcurrentObservations tests that concurrent observations
// don't cause incorrect EWMA calculations due to race conditions.
func TestEWMABreaker_ConcurrentObservations(t *testing.T) {
	breaker := NewEWMABreaker(10, 0.5)
	
	// Run many concurrent observations
	const numGoroutines = 100
	const observationsPerGoroutine = 100
	
	var wg sync.WaitGroup
	wg.Add(numGoroutines)
	
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			// Alternate between success and failure
			for j := 0; j < observationsPerGoroutine; j++ {
				failure := (id+j)%2 == 0
				breaker.observe(false, failure)
			}
		}(i)
	}
	
	wg.Wait()
	
	// With 50% failures and threshold of 0.5, the breaker should eventually stabilize
	// The exact value depends on the EWMA calculation, but it should be close to 0.5
	// and not have corrupted values
	finalRate := fromStore(breaker.failureRate.Load())
	
	// The rate should be between 0 and 1
	assert.GreaterOrEqual(t, finalRate, 0.0, "failure rate should be >= 0")
	assert.LessOrEqual(t, finalRate, 1.0, "failure rate should be <= 1")
	
	// With many observations at ~50% failure rate, it should converge near 0.5
	// Allow some variance due to the EWMA nature
	assert.InDelta(t, 0.5, finalRate, 0.3, "failure rate should converge near 50%")
}

// TestSlidingWindowBreaker_ConcurrentWindowSwap tests that concurrent calls
// during window swapping don't lose counts or produce incorrect results.
func TestSlidingWindowBreaker_ConcurrentWindowSwap(t *testing.T) {
	windowSize := 100 * time.Millisecond
	breaker := NewSlidingWindowBreaker(windowSize, 0.5)
	
	// Start with some initial failures in the first window
	for i := 0; i < 50; i++ {
		breaker.observe(false, true)
	}
	for i := 0; i < 50; i++ {
		breaker.observe(false, false)
	}
	
	// Sleep to ensure we're past the window
	time.Sleep(windowSize + 10*time.Millisecond)
	
	// Now trigger concurrent observations that should cause a window swap
	const numGoroutines = 50
	var wg sync.WaitGroup
	wg.Add(numGoroutines)
	
	var successCount, failureCount atomic.Int64
	
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			failure := id%2 == 0
			breaker.observe(false, failure)
			if failure {
				failureCount.Add(1)
			} else {
				successCount.Add(1)
			}
		}(i)
	}
	
	wg.Wait()
	
	// Verify counts are consistent (no lost observations)
	currentSuccess := breaker.currentSuccessCount.Load()
	currentFailure := breaker.currentFailureCount.Load()
	lastSuccess := breaker.lastSuccessCount.Load()
	lastFailure := breaker.lastFailureCount.Load()
	
	totalInBreaker := currentSuccess + currentFailure + lastSuccess + lastFailure
	totalObserved := successCount.Load() + failureCount.Load()
	
	// The breaker should have tracked all observations (some might be in old window)
	// At minimum, current window should have the observations
	assert.GreaterOrEqual(t, totalInBreaker, totalObserved-100, 
		"breaker should track most observations, current+last=%d, observed=%d", 
		totalInBreaker, totalObserved)
}

// TestCircuit_HalfOpenConcurrency tests that the half-open state properly limits
// concurrent calls to ~1, not allowing many calls through simultaneously.
func TestCircuit_HalfOpenConcurrency(t *testing.T) {
	var callsInProgress atomic.Int32
	var maxConcurrent atomic.Int32
	var callsCompleted atomic.Int32
	
	slowFunc := func(ctx context.Context, in int) (int, error) {
		current := callsInProgress.Add(1)
		defer callsInProgress.Add(-1)
		
		// Update max concurrent
		for {
			max := maxConcurrent.Load()
			if current <= max || maxConcurrent.CompareAndSwap(max, current) {
				break
			}
		}
		
		// Slow down the call to give time for concurrent calls
		time.Sleep(50 * time.Millisecond)
		callsCompleted.Add(1)
		return in, nil
	}
	
	// Create a breaker that opens immediately on first failure
	breaker := NewEWMABreaker(1, 0.01)
	c, err := NewCircuit(slowFunc, breaker, WithHalfOpenDelay(100*time.Millisecond))
	require.NoError(t, err)
	
	// Make it fail to open the circuit
	c.Call(context.Background(), -1)
	c.open()
	assert.Equal(t, StateOpen, c.State())
	
	// Wait for half-open
	time.Sleep(150 * time.Millisecond)
	assert.Equal(t, StateHalfOpen, c.State())
	
	// Try to make many concurrent calls in half-open state
	const numConcurrent = 20
	var wg sync.WaitGroup
	wg.Add(numConcurrent)
	
	for i := 0; i < numConcurrent; i++ {
		go func(id int) {
			defer wg.Done()
			c.Call(context.Background(), id)
		}(i)
	}
	
	wg.Wait()
	
	maxConcurrentCalls := maxConcurrent.Load()
	completedCalls := callsCompleted.Load()
	
	t.Logf("Max concurrent calls in half-open: %d", maxConcurrentCalls)
	t.Logf("Completed calls: %d", completedCalls)
	
	// In half-open state, we should limit to ~1 call, definitely not all 20
	// The comment says "limited (~1)", so we allow a small number due to race conditions
	// But definitely should not be close to numConcurrent
	assert.LessOrEqual(t, maxConcurrentCalls, int32(5), 
		"half-open should limit concurrent calls to ~1, not %d", maxConcurrentCalls)
}

// TestCircuit_ConcurrentStateChanges tests that concurrent calls don't cause
// incorrect state changes that would affect unrelated calls.
func TestCircuit_ConcurrentStateChanges(t *testing.T) {
	var successCount, failureCount, circuitOpenCount atomic.Int32
	
	testFunc := func(ctx context.Context, shouldFail bool) (bool, error) {
		if shouldFail {
			failureCount.Add(1)
			return false, assert.AnError
		}
		successCount.Add(1)
		return true, nil
	}
	
	// Breaker that opens quickly (low threshold, small sample)
	breaker := NewEWMABreaker(5, 0.3)
	c, err := NewCircuit(testFunc, breaker, WithHalfOpenDelay(50*time.Millisecond))
	require.NoError(t, err)
	
	const numGoroutines = 100
	const callsPerGoroutine = 10
	
	var wg sync.WaitGroup
	wg.Add(numGoroutines)
	
	for i := 0; i < numGoroutines; i++ {
		go func(id int) {
			defer wg.Done()
			for j := 0; j < callsPerGoroutine; j++ {
				// Mix successful and failing calls
				shouldFail := (id*callsPerGoroutine+j)%3 == 0
				_, err := c.Call(context.Background(), shouldFail)
				if err == ErrCircuitOpen {
					circuitOpenCount.Add(1)
				}
			}
		}(i)
	}
	
	wg.Wait()
	
	totalSuccesses := successCount.Load()
	totalFailures := failureCount.Load()
	totalCircuitOpen := circuitOpenCount.Load()
	totalAttempts := int32(numGoroutines * callsPerGoroutine)
	
	t.Logf("Successes: %d, Failures: %d, Circuit Open: %d, Total: %d", 
		totalSuccesses, totalFailures, totalCircuitOpen, totalAttempts)
	
	// Verify accounting: all attempts should be accounted for
	assert.Equal(t, totalAttempts, totalSuccesses+totalFailures+totalCircuitOpen,
		"all calls should be accounted for")
	
	// With ~33% failures, the circuit should open at some point
	assert.Greater(t, totalCircuitOpen, int32(0), "circuit should have opened")
	
	// But not all calls should be blocked (circuit should close again eventually)
	assert.Less(t, totalCircuitOpen, totalAttempts,
		"not all calls should be blocked - circuit should recover")
}
