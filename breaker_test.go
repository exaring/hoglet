package hoglet

import (
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestEWMABreaker_zero_value_does_not_open(t *testing.T) {
	b := &EWMABreaker{}
	o := b.Call()
	assert.NotNil(t, o)
	o.Observe(true)
	assert.NotNil(t, b.Call())
}

func TestEWMABreaker_zero_value_does_not_panic(t *testing.T) {
	b := &EWMABreaker{}
	assert.NotPanics(t, func() { b.Call() })
}

func TestBreaker_Observe_State(t *testing.T) {
	halfOpenDelay := 500 * time.Millisecond
	// helper functions to make tests stages more readable
	alwaysFailure := func(int) bool { return true }
	alwaysSuccessful := func(int) bool { return false }

	type stages struct {
		calls       int
		failureFunc func(int) bool
	}
	tests := []struct {
		name     string
		breakers map[string]Breaker
		stages   []stages
		wantCall bool
	}{
		{
			name: "start closed",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(10, 0.3, halfOpenDelay),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.3, halfOpenDelay),
			},
			wantCall: true,
		},
		{
			name: "always success",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(10, 0.3, halfOpenDelay),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.3, halfOpenDelay),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysSuccessful},
			},
			wantCall: true,
		},
		{
			name: "always failure",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(10, 0.9, halfOpenDelay),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.9, halfOpenDelay),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure},
			},
			wantCall: false,
		},
		{
			name: "start open; finish closed",
			breakers: map[string]Breaker{
				"ewma": NewEWMABreaker(10, 0.2, halfOpenDelay),
				// sliding window is not affected by ordering
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure},
				{calls: 100, failureFunc: alwaysSuccessful},
			},
			wantCall: true,
		},
		{
			name: "start closed; finish open",
			breakers: map[string]Breaker{
				"ewma": NewEWMABreaker(50, 0.4, halfOpenDelay),
				// sliding window is not affected by ordering
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysSuccessful},
				{calls: 100, failureFunc: alwaysFailure},
			},
			wantCall: false,
		},
		{
			name: "just above threshold opens",
			breakers: map[string]Breaker{
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.5, halfOpenDelay),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysSuccessful},
				{calls: 101, failureFunc: alwaysFailure},
			},
			wantCall: false,
		},
		{
			name: "just below threshold stays closed",
			breakers: map[string]Breaker{
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.5, halfOpenDelay),
			},
			stages: []stages{
				{calls: 101, failureFunc: alwaysSuccessful},
				{calls: 100, failureFunc: alwaysFailure},
			},
			wantCall: true,
		},
		{
			name: "constant low failure rate stays mostly closed (EWMA flaky)",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(50, 0.2, halfOpenDelay),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.2, halfOpenDelay),
			},
			stages: []stages{
				{calls: 100, failureFunc: func(int) bool { return rand.Float64() < 0.1 }},
			},
			wantCall: true,
		},
		{
			name: "constant high failure rate stays mostly open (EWMA flaky)",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(50, 0.2, halfOpenDelay),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.2, halfOpenDelay),
			},
			stages: []stages{
				{calls: 100, failureFunc: func(int) bool { return rand.Float64() < 0.4 }},
			},
			wantCall: false,
		},
		{
			name: "single success at half-open enough to close",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(50, 0.1, halfOpenDelay),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.1, halfOpenDelay),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure},
				{calls: 1, failureFunc: func(int) bool {
					time.Sleep(halfOpenDelay)
					return false
				}},
			},
			wantCall: true,
		},
		{
			name: "single failure at half-open keeps open",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(50, 0.1, halfOpenDelay),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.1, halfOpenDelay),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure},
				{calls: 1, failureFunc: func(int) bool {
					time.Sleep(halfOpenDelay)
					return true
				}},
			},
			wantCall: false,
		},
		{
			// we want to re-open fast if we closed on a fluke (to avoid thundering herd agains a service that might be
			// close to capacity and therefore failing intermittently)
			name: "single failure after reopen closes",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(50, 0.1, halfOpenDelay),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.1, halfOpenDelay),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure},
				{calls: 1, failureFunc: func(int) bool {
					time.Sleep(halfOpenDelay)
					return false
				}},
				{calls: 1, failureFunc: alwaysFailure},
			},
			wantCall: false,
		},
	}
	for _, tt := range tests {
		tt := tt
		for bName, b := range tt.breakers {
			b := b
			t.Run(bName+": "+tt.name, func(t *testing.T) {
				t.Parallel()

				for _, s := range tt.stages {
					for i := 0; i < s.calls; i++ {
						failure := s.failureFunc(i)
						o := b.Call()

						switch b := b.(type) {
						case *EWMABreaker:
							if o != nil {
								o.Observe(failure)
							} else {
								b.observe(false, failure) // we're testing just the breaker
							}
							t.Logf("%s: sample %d: failure %v: failureRate %f => %v", tt.name, i, failure, b.failureRate.Load(), b.state())
						case *SlidingWindowBreaker:
							if o != nil {
								o.Observe(failure)
							} else {
								b.observe(false, failure) // we're testing just the breaker
							}
							t.Logf("%s: sample %d: failure %v: => %v", tt.name, i, failure, b.state())
						}
					}
				}
				if tt.wantCall {
					assert.NotNil(t, b.Call())
				} else {
					assert.Nil(t, b.Call())
				}
			})
		}
	}
}
