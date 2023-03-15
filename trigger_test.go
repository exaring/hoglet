package hoglet

import (
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestEWMATrigger_zero_value_does_not_open(t *testing.T) {
	o := &EWMATrigger{}
	o.Observe(true)
	assert.Equal(t, StateClosed, o.State())
}

func TestEWMATrigger_zero_value_does_not_panic(t *testing.T) {
	o := &EWMATrigger{}
	assert.NotPanics(t, func() { o.State() })
}

func TestEWMATrigger_Observe_State(t *testing.T) {
	halfOpenDelay := 500 * time.Millisecond
	// helper functions to make tests stages more readable
	alwaysFailure := func(int) bool { return true }
	alwaysSuccessful := func(int) bool { return false }

	type stages struct {
		calls       int
		failureFunc func(int) bool
	}
	tests := []struct {
		name      string
		triggers  []Trigger
		stages    []stages
		wantState State
	}{
		{
			name: "start closed",
			triggers: []Trigger{
				NewEWMATrigger(10, 0.3, halfOpenDelay),
				NewSlidingWindowTrigger(10*time.Second, 0.3, halfOpenDelay),
			},
			wantState: StateClosed,
		},
		{
			name: "always success",
			triggers: []Trigger{
				NewEWMATrigger(10, 0.3, halfOpenDelay),
				NewSlidingWindowTrigger(10*time.Second, 0.3, halfOpenDelay),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysSuccessful},
			},
			wantState: StateClosed,
		},
		{
			name: "always failure",
			triggers: []Trigger{
				NewEWMATrigger(10, 0.9, halfOpenDelay),
				NewSlidingWindowTrigger(10*time.Second, 0.9, halfOpenDelay),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure},
			},
			wantState: StateOpen,
		},
		{
			name: "start open; finish closed",
			triggers: []Trigger{
				NewEWMATrigger(10, 0.2, halfOpenDelay),
				// sliding window is not affected by ordering
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure},
				{calls: 100, failureFunc: alwaysSuccessful},
			},
			wantState: StateClosed,
		},
		{
			name: "start closed; finish open",
			triggers: []Trigger{
				NewEWMATrigger(50, 0.4, halfOpenDelay),
				// sliding window is not affected by ordering
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysSuccessful},
				{calls: 100, failureFunc: alwaysFailure},
			},
			wantState: StateOpen,
		},
		{
			name: "just above threshold opens",
			triggers: []Trigger{
				NewSlidingWindowTrigger(10*time.Second, 0.5, halfOpenDelay),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysSuccessful},
				{calls: 101, failureFunc: alwaysFailure},
			},
			wantState: StateOpen,
		},
		{
			name: "just below threshold stays closed",
			triggers: []Trigger{
				NewSlidingWindowTrigger(10*time.Second, 0.5, halfOpenDelay),
			},
			stages: []stages{
				{calls: 101, failureFunc: alwaysSuccessful},
				{calls: 100, failureFunc: alwaysFailure},
			},
			wantState: StateClosed,
		},
		{
			name: "constant low failure rate stays mostly closed (EWMA flaky)",
			triggers: []Trigger{
				NewEWMATrigger(50, 0.2, halfOpenDelay),
				NewSlidingWindowTrigger(10*time.Second, 0.2, halfOpenDelay),
			},
			stages: []stages{
				{calls: 100, failureFunc: func(int) bool { return rand.Float64() < 0.1 }},
			},
			wantState: StateClosed,
		},
		{
			name: "constant high failure rate stays mostly open (EWMA flaky)",
			triggers: []Trigger{
				NewEWMATrigger(50, 0.2, halfOpenDelay),
				NewSlidingWindowTrigger(10*time.Second, 0.2, halfOpenDelay),
			},
			stages: []stages{
				{calls: 100, failureFunc: func(int) bool { return rand.Float64() < 0.4 }},
			},
			wantState: StateOpen,
		},
		{
			name: "single success at half-open enough to close",
			triggers: []Trigger{
				NewEWMATrigger(50, 0.1, halfOpenDelay),
				NewSlidingWindowTrigger(10*time.Second, 0.1, halfOpenDelay),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure},
				{calls: 1, failureFunc: func(int) bool {
					time.Sleep(halfOpenDelay)
					return false
				}},
			},
			wantState: StateClosed,
		},
		{
			name: "single failure at half-open keeps open",
			triggers: []Trigger{
				NewEWMATrigger(50, 0.1, halfOpenDelay),
				NewSlidingWindowTrigger(10*time.Second, 0.1, halfOpenDelay),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure},
				{calls: 1, failureFunc: func(int) bool {
					time.Sleep(halfOpenDelay)
					return true
				}},
			},
			wantState: StateOpen,
		},
		{
			// we want to re-open fast if we closed on a fluke (to avoid thundering herd agains a service that might be
			// close to capacity and therefore failing intermittently)
			name: "single failure after reopen closes",
			triggers: []Trigger{
				NewEWMATrigger(50, 0.1, halfOpenDelay),
				NewSlidingWindowTrigger(10*time.Second, 0.1, halfOpenDelay),
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure},
				{calls: 1, failureFunc: func(int) bool {
					time.Sleep(halfOpenDelay)
					return false
				}},
				{calls: 1, failureFunc: alwaysFailure},
			},
			wantState: StateOpen,
		},
	}
	for _, tt := range tests {
		tt := tt
		for _, ttt := range tt.triggers {
			ttt := ttt
			t.Run(tt.name, func(t *testing.T) {
				t.Parallel()

				for _, s := range tt.stages {
					for i := 0; i < s.calls; i++ {
						failure := s.failureFunc(i)
						ttt.Observe(failure)
						// t.Logf("%s: sample %d: failure %v: successRate %f => %v", tt.name, i, failure, e.successRate.Load(), e.State())
					}
				}
				assert.Equal(t, tt.wantState, ttt.State())
			})
		}
	}
}
