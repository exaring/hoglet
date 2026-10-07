package hoglet

import (
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEWMABreaker_zero_value_does_not_open(t *testing.T) {
	b := &EWMABreaker{}
	s := b.observe(false, true)
	assert.NotEqual(t, stateChangeOpen, s)
}

func TestEWMABreaker_zero_value_does_not_panic(t *testing.T) {
	b := &EWMABreaker{}
	assert.NotPanics(t, func() {
		b.observe(false, true) // nolint: errcheck // we are just interested in the panic
	})
}

func TestEWMABreaker_sample_count_of_1_is_single_sample(t *testing.T) {
	// sampleCount == 1 must yield decay == 1 (only the newest sample counts), i.e. a valid in-range smoothing factor.
	b := NewEWMABreaker(1, 0.5)
	assert.Equal(t, 1.0, b.decay)
}

func TestEWMABreaker_sample_count_of_0_is_rejected(t *testing.T) {
	_, err := NewCircuit(NewEWMABreaker(0, 0.5), WithHalfOpenDelay(time.Second))
	assert.Error(t, err, "expected a sample count of 0 to be rejected")
}

func TestEWMABreaker_single_failure_never_opens_healthy_breaker(t *testing.T) {
	// A long healthy run decays the rate down into the subnormal floats. At no point along the way may a single
	// failure be taken for the very first sample (i.e. a failure rate of 1): the "unobserved" sentinel must not be
	// reachable by arithmetic.
	b := NewEWMABreaker(3, 0.9)
	b.observe(false, true)
	for i := range 2000 {
		b.observe(false, false)

		probe := NewEWMABreaker(3, 0.9)
		probe.failureRate.Store(b.failureRate.Load())
		require.Equal(t, stateChangeClose, probe.observe(false, true), "single failure after %d successes", i+1)
	}
}

func TestEWMABreaker_concurrent_observations_do_not_open_healthy_breaker(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test")
	}

	// 1% failures against a 50% threshold: any open is spurious. Updating the rate without a CompareAndSwap lets a
	// concurrent observer read another one's raw sample as the previous rate, wiping the history on a single failure.
	b := NewEWMABreaker(50, 0.5)
	var opens atomic.Int64
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g)))
			for range 50_000 {
				if b.observe(false, rng.Float64() < 0.01) == stateChangeOpen {
					opens.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	assert.Zero(t, opens.Load(), "spurious opens")
}

// testSeed is a fixed seed for the per-subtest RNG so the statistically-driven EWMA stages are deterministic and
// reproducible across runs (chosen so the "constant low/high failure rate" cases land on their expected state).
const testSeed = 0

func TestBreaker_Observe_State(t *testing.T) {
	// helper functions to make tests stages more readable
	alwaysFailure := func(*rand.Rand, int) bool { return true }
	alwaysSuccessful := func(*rand.Rand, int) bool { return false }

	type stages struct {
		calls           int
		failureFunc     func(r *rand.Rand, call int) bool
		waitForHalfOpen bool        // whether to put circuit in half-open BEFORE observing the call's result
		wantStateChange stateChange // expected state change at the END of the stage
	}
	tests := []struct {
		name     string
		breakers map[string]Breaker
		stages   []stages
	}{
		{
			name: "start closed",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(10, 0.3),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.3),
			},
			stages: []stages{
				{calls: 1, failureFunc: alwaysSuccessful, wantStateChange: stateChangeClose},
			},
		},
		{
			name: "always success",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(10, 0.3),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.3),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysSuccessful, wantStateChange: stateChangeClose},
			},
		},
		{
			name: "always failure",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(10, 0.9),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.9),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure, wantStateChange: stateChangeOpen},
			},
		},
		{
			name: "start open; finish closed",
			breakers: map[string]Breaker{
				"ewma": NewEWMABreaker(10, 0.2),
				// sliding window is not affected by ordering
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure, wantStateChange: stateChangeOpen},
				{calls: 100, failureFunc: alwaysSuccessful, wantStateChange: stateChangeClose},
			},
		},
		{
			name: "start closed; finish open",
			breakers: map[string]Breaker{
				"ewma": NewEWMABreaker(50, 0.4),
				// sliding window is not affected by ordering
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysSuccessful, wantStateChange: stateChangeClose},
				{calls: 100, failureFunc: alwaysFailure, wantStateChange: stateChangeOpen},
			},
		},
		{
			name: "just above threshold opens",
			breakers: map[string]Breaker{
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.5),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysSuccessful, wantStateChange: stateChangeClose},
				{calls: 101, failureFunc: alwaysFailure, wantStateChange: stateChangeOpen},
			},
		},
		{
			name: "just below threshold stays closed",
			breakers: map[string]Breaker{
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.5),
			},
			stages: []stages{
				{calls: 101, failureFunc: alwaysSuccessful, wantStateChange: stateChangeClose},
				{calls: 100, failureFunc: alwaysFailure, wantStateChange: stateChangeClose},
			},
		},
		{
			name: "constant low failure rate stays mostly closed",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(50, 0.2),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.2),
			},
			stages: []stages{
				{calls: 100, failureFunc: func(r *rand.Rand, _ int) bool { return r.Float64() < 0.1 }, wantStateChange: stateChangeClose},
			},
		},
		{
			name: "constant high failure rate stays mostly open",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(50, 0.2),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.2),
			},
			stages: []stages{
				{calls: 100, failureFunc: func(r *rand.Rand, _ int) bool { return r.Float64() < 0.4 }, wantStateChange: stateChangeOpen},
			},
		},
		{
			// failures from before the circuit opened must not reopen it right after a successful half-open call
			name: "single success at half-open enough to close for good",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(50, 0.1),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.1),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure, wantStateChange: stateChangeOpen},
				{calls: 1, failureFunc: alwaysSuccessful, waitForHalfOpen: true, wantStateChange: stateChangeClose},
				{calls: 10, failureFunc: alwaysSuccessful, wantStateChange: stateChangeClose},
			},
		},
		{
			name: "single failure at half-open keeps open",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(50, 0.1),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.1),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure, wantStateChange: stateChangeOpen},
				{calls: 1, failureFunc: alwaysFailure, waitForHalfOpen: true, wantStateChange: stateChangeOpen},
			},
		},
		{
			// we want to re-open fast if we closed on a fluke (to avoid thundering herd agains a service that might be
			// close to capacity and therefore failing intermittently)
			name: "single failure after reopen closes",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(50, 0.1),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.1),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure, wantStateChange: stateChangeOpen},
				{calls: 1, failureFunc: alwaysSuccessful, waitForHalfOpen: true, wantStateChange: stateChangeClose},
				{calls: 1, failureFunc: alwaysFailure, wantStateChange: stateChangeOpen},
			},
		},
	}
	for _, tt := range tests {
		for bName, b := range tt.breakers {
			t.Run(bName+": "+tt.name, func(t *testing.T) {
				t.Parallel()

				// per-subtest RNG with a fixed seed: deterministic and independent of other parallel subtests.
				rng := rand.New(rand.NewSource(testSeed))

				for _, s := range tt.stages {
					var lastStateChange stateChange

					for i := 1; i <= s.calls; i++ {
						failure := s.failureFunc(rng, i)
						switch b := b.(type) {
						case *EWMABreaker:
							lastStateChange = ignoreNone(lastStateChange, b.observe(s.waitForHalfOpen && i == s.calls, failure))
							// t.Logf("%s: sample %d: failure %v: failureRate %f => %v", tt.name, i, failure, b.failureRate.Load(), b.circuit.State())
						case *SlidingWindowBreaker:
							lastStateChange = ignoreNone(lastStateChange, b.observe(s.waitForHalfOpen && i == s.calls, failure))
							// t.Logf("%s: sample %d: failure %v: => %v", tt.name, i, failure, b.circuit.State())
						}
					}

					assert.Equal(t, s.wantStateChange, lastStateChange, "expected %q, got %q", s.wantStateChange, lastStateChange)
				}
			})
		}
	}
}

func TestSlidingWindowBreaker_window_start_is_stable_within_window(t *testing.T) {
	b := NewSlidingWindowBreaker(time.Minute, 0.5)

	b.observe(false, false) // first observation initializes the window
	windowStart := b.currentStart.Load()
	assert.NotZero(t, windowStart)

	b.observe(false, false)
	b.observe(false, true)
	assert.Equal(t, windowStart, b.currentStart.Load(), "observations within the window must not move its start")
}

func TestSlidingWindowBreaker_rotates_windows_after_windowSize(t *testing.T) {
	b := NewSlidingWindowBreaker(time.Minute, 0.1)

	assert.Equal(t, stateChangeOpen, b.observe(false, true))

	// simulate passage of time: pretend the current window started more than a windowSize ago
	b.currentStart.Store(nowNanos() - int64(b.windowSize+time.Second))

	// the failure rotated into the last window by this very observation must count towards its failure rate
	assert.Equal(t, stateChangeOpen, b.observe(false, false))
	assert.EqualValues(t, 1, b.lastFailureCount.Load(), "failures should have been rotated into the last window")
	assert.EqualValues(t, 0, b.currentFailureCount.Load())
}

func TestSlidingWindowBreaker_last_window_weighs_its_remaining_overlap(t *testing.T) {
	b := NewSlidingWindowBreaker(time.Minute, 0.4)

	assert.Equal(t, stateChangeOpen, b.observe(false, true))

	// simulate passage of time: the window ended half a window ago, so only half of it is still visible
	b.currentStart.Store(nowNanos() - int64(b.windowSize*3/2))

	// ~0.5 weighted failures out of ~1.5 weighted calls is below the threshold
	assert.Equal(t, stateChangeClose, b.observe(false, false))
}

func TestSlidingWindowBreaker_drops_windows_older_than_two_windowSizes(t *testing.T) {
	b := NewSlidingWindowBreaker(time.Minute, 0.1)

	assert.Equal(t, stateChangeOpen, b.observe(false, true))

	// simulate passage of time: both windows passed without observations
	b.currentStart.Store(nowNanos() - int64(2*b.windowSize+time.Second))

	assert.Equal(t, stateChangeClose, b.observe(false, false))
	assert.EqualValues(t, 0, b.lastFailureCount.Load(), "outdated failures should have been dropped")
}

func TestSlidingWindowBreaker_concurrent_rotation_weighs_last_window(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const goroutines = 8
		b := NewSlidingWindowBreaker(time.Second, 0.5)

		// Every round, all goroutines observe at once right after the window has passed, so they race to rotate it.
		// Time is frozen meanwhile, so they all measure the same elapsed time. Whoever loses the rotation must still
		// weigh the window that just ended (healthy: one failure per round), or its own failure alone is judged over
		// the fresh window, i.e. a failure rate of 1.
		for round := range 100 {
			time.Sleep(time.Second + time.Microsecond)

			start := make(chan struct{})
			results := make(chan stateChange, goroutines)
			for g := range goroutines {
				go func() {
					<-start
					// round 0 only warms up the window, since nothing precedes the very first one
					results <- b.observe(false, round > 0 && g == round%goroutines)
				}()
			}
			close(start)

			for range goroutines {
				assert.NotEqual(t, stateChangeOpen, <-results, "round %d", round)
			}
		}
	})
}

// ignoreNone is a small helper to skip the "none" state change and only record the last "effective" state change.
func ignoreNone(old, new stateChange) stateChange {
	if new == stateChangeNone {
		return old
	}
	return new
}
