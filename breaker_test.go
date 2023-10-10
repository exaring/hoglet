package hoglet

import (
	"context"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEWMABreaker_zero_value_does_not_open(t *testing.T) {
	b := &EWMABreaker{}
	b.connect(&mockCircuit{})
	o, err := b.observerForCall(context.TODO())
	require.NoError(t, err)
	o.observe(true)
	_, err = b.observerForCall(context.TODO())
	assert.NoError(t, err)
}

func TestEWMABreaker_zero_value_does_not_panic(t *testing.T) {
	b := &EWMABreaker{}
	b.connect(&mockCircuit{})
	assert.NotPanics(t, func() {
		b.observerForCall(context.TODO()) // nolint: errcheck // we are just interested in the panic
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
		name     string
		breakers map[string]Breaker
		stages   []stages
		wantCall bool
	}{
		{
			name: "start closed",
			breakers: map[string]Breaker{
				"ewma":          NewEWMABreaker(10, 0.3),
				"slidingwindow": NewSlidingWindowBreaker(10*time.Second, 0.3),
			},
			wantCall: true,
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
			wantCall: true,
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
			wantCall: false,
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
			wantCall: true,
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
			wantCall: false,
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
			wantCall: false,
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
			wantCall: true,
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
			wantCall: true,
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
			wantCall: false,
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
			wantCall: true,
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
			wantCall: false,
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
			wantCall: false,
		},
	}
	for _, tt := range tests {
		tt := tt
		for bName, b := range tt.breakers {
			b := b
			c := &mockCircuit{}
			b.connect(c)
			t.Run(bName+": "+tt.name, func(t *testing.T) {
				t.Parallel()

				for _, s := range tt.stages {
					for i := 0; i < s.calls; i++ {
						if s.waitForHalfOpen {
							c.setState(StateHalfOpen)
						}
						failure := s.failureFunc(i)
						o, _ := b.observerForCall(context.TODO()) // nolint: errcheck // always observe

						switch b := b.(type) {
						case *EWMABreaker:
							if o != nil {
								o.observe(failure)
							} else {
								b.observe(s.waitForHalfOpen, failure)
							}
							// t.Logf("%s: sample %d: failure %v: failureRate %f => %v", tt.name, i, failure, b.failureRate.Load(), b.circuit.State())
						case *SlidingWindowBreaker:
							if o != nil {
								o.observe(failure)
							} else {
								b.observe(s.waitForHalfOpen, failure)
							}
							// t.Logf("%s: sample %d: failure %v: => %v", tt.name, i, failure, b.circuit.State())
						}
					}
				}
				_, err := b.observerForCall(context.TODO())
				if tt.wantCall {
					assert.NoError(t, err)
				} else {
					assert.ErrorIs(t, err, ErrCircuitOpen)
				}
			})
		}
	}
}

type mockCircuit struct {
	state    State
	openedAt int64
}

func (m *mockCircuit) stateForCall() State {
	return m.state
}

func (m *mockCircuit) setOpenedAt(t int64) {
	if t != 0 {
		m.state = StateOpen
	} else {
		m.state = StateClosed
	}
	m.openedAt = t
}

func (m *mockCircuit) setState(s State) {
	m.state = s
}
