package hoglet

import (
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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

func TestBreaker_Observe_State(t *testing.T) {
	// helper functions to make tests stages more readable
	alwaysFailure := func(int) bool { return true }
	alwaysSuccessful := func(int) bool { return false }

	type stages struct {
		calls           int
		failureFunc     func(int) bool
		waitForHalfOpen bool // whether to put circuit in half-open BEFORE observing the call's result
	}
	tests := []struct {
		name      string
		breakers  map[string]Breaker
		stages    []stages
		wantState stateChange
	}{
		{
			name: "start closed",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(10, 0.3),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.3),
			},
			wantState: stateChangeNone,
		},
		{
			name: "always success",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(10, 0.3),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.3),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysSuccessful},
			},
			wantState: stateChangeClose,
		},
		{
			name: "always failure",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(10, 0.9),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.9),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure},
			},
			wantState: stateChangeOpen,
		},
		{
			name: "start open; finish closed",
			breakers: map[string]Breaker{
				"ewma": NewEWMABreaker(10, 0.2),
				// sliding window is not affected by ordering
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure},
				{calls: 100, failureFunc: alwaysSuccessful},
			},
			wantState: stateChangeClose,
		},
		{
			name: "start closed; finish open",
			breakers: map[string]Breaker{
				"ewma": NewEWMABreaker(50, 0.4),
				// sliding window is not affected by ordering
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysSuccessful},
				{calls: 100, failureFunc: alwaysFailure},
			},
			wantState: stateChangeOpen,
		},
		{
			name: "just above threshold opens",
			breakers: map[string]Breaker{
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.5),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysSuccessful},
				{calls: 101, failureFunc: alwaysFailure},
			},
			wantState: stateChangeOpen,
		},
		{
			name: "just below threshold stays closed",
			breakers: map[string]Breaker{
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.5),
			},
			stages: []stages{
				{calls: 101, failureFunc: alwaysSuccessful},
				{calls: 100, failureFunc: alwaysFailure},
			},
			wantState: stateChangeClose,
		},
		{
			name: "constant low failure rate stays mostly closed (EWMA flaky)",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(50, 0.2),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.2),
			},
			stages: []stages{
				{calls: 100, failureFunc: func(int) bool { return rand.Float64() < 0.1 }},
			},
			wantState: stateChangeClose,
		},
		{
			name: "constant high failure rate stays mostly open (EWMA flaky)",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(50, 0.2),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.2),
			},
			stages: []stages{
				{calls: 100, failureFunc: func(int) bool { return rand.Float64() < 0.4 }},
			},
			wantState: stateChangeOpen,
		},
		{
			name: "single success at half-open enough to close",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(50, 0.1),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.1),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure},
				{calls: 1, failureFunc: alwaysSuccessful, waitForHalfOpen: true},
			},
			wantState: stateChangeClose,
		},
		{
			name: "single failure at half-open keeps open",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(50, 0.1),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.1),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure},
				{calls: 1, failureFunc: alwaysFailure, waitForHalfOpen: true},
			},
			wantState: stateChangeOpen,
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
				{calls: 100, failureFunc: alwaysFailure},
				{calls: 1, failureFunc: alwaysSuccessful, waitForHalfOpen: true},
				{calls: 1, failureFunc: alwaysFailure},
			},
			wantState: stateChangeOpen,
		},
	}
	for _, tt := range tests {
		tt := tt
		for bName, b := range tt.breakers {
			b := b
			t.Run(bName+": "+tt.name, func(t *testing.T) {
				t.Parallel()

				var lastStateChange stateChange

				for _, s := range tt.stages {
					for i := 1; i <= s.calls; i++ {
						failure := s.failureFunc(i)
						switch b := b.(type) {
						case *EWMABreaker:
							lastStateChange = ignoreNone(lastStateChange, b.observe(s.waitForHalfOpen && i == s.calls, failure))
							// t.Logf("%s: sample %d: failure %v: failureRate %f => %v", tt.name, i, failure, b.failureRate.Load(), b.circuit.State())
						case *SlidingWindowBreaker:
							lastStateChange = ignoreNone(lastStateChange, b.observe(s.waitForHalfOpen && i == s.calls, failure))
							// t.Logf("%s: sample %d: failure %v: => %v", tt.name, i, failure, b.circuit.State())
						}
					}
				}
				assert.Equal(t, tt.wantState, lastStateChange)
			})
		}
	}
}

func ignoreNone(old, new stateChange) stateChange {
	if new == stateChangeNone {
		return old
	}
	return new
}
