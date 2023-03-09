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
	assert.Equal(t, o.State(), StateClosed)
}

func TestEWMATrigger_Observe_State(t *testing.T) {
	// helper functions to make tests stages more readable
	alwaysFailure := func(int) bool { return true }
	alwaysSuccessful := func(int) bool { return false }

	type fields struct {
		sampleCount int
		threshold   float64
	}
	type stages struct {
		calls       int
		failureFunc func(int) bool
	}
	tests := []struct {
		name      string
		fields    fields
		stages    []stages
		wantState State
	}{
		{
			name: "start closed",
			fields: fields{
				sampleCount: 10,
				threshold:   0.7,
			},
			wantState: StateClosed,
		},
		{
			name: "always success",
			fields: fields{
				sampleCount: 10,
				threshold:   0.7,
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysSuccessful},
			},
			wantState: StateClosed,
		},
		{
			name: "always failure",
			fields: fields{
				sampleCount: 10,
				threshold:   0.1,
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure},
			},
			wantState: StateOpen,
		},
		{
			name: "start open; finish closed",
			fields: fields{
				sampleCount: 10,
				threshold:   0.8,
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure},
				{calls: 100, failureFunc: alwaysSuccessful},
			},
			wantState: StateClosed,
		},
		{
			name: "start closed; finish open",
			fields: fields{
				sampleCount: 50,
				threshold:   0.5,
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysSuccessful},
				{calls: 100, failureFunc: alwaysFailure},
			},
			wantState: StateOpen,
		},
		{
			name: "constant low failure rate stays mostly closed (flaky)",
			fields: fields{
				sampleCount: 50,
				threshold:   0.8,
			},
			stages: []stages{
				{calls: 100, failureFunc: func(int) bool { return rand.Float64() < 0.1 }},
			},
			wantState: StateClosed,
		},
		{
			name: "constant high failure rate stays mostly open (flaky)",
			fields: fields{
				sampleCount: 50,
				threshold:   0.8,
			},
			stages: []stages{
				{calls: 100, failureFunc: func(int) bool { return rand.Float64() < 0.4 }},
			},
			wantState: StateOpen,
		},
		{
			name: "single success at half-open enough to close",
			fields: fields{
				sampleCount: 50,
				threshold:   0.9,
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure},
				{calls: 1, failureFunc: func(int) bool {
					time.Sleep(500 * time.Millisecond)
					return false
				}},
			},
			wantState: StateClosed,
		},
		{
			name: "single failure at half-open keeps open",
			fields: fields{
				sampleCount: 50,
				threshold:   1,
			},
			stages: []stages{
				{calls: 100, failureFunc: alwaysFailure},
				{calls: 1, failureFunc: func(int) bool {
					time.Sleep(500 * time.Millisecond)
					return true
				}},
			},
			wantState: StateOpen,
		},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			e := NewEWMATrigger(tt.fields.sampleCount, tt.fields.threshold, 500*time.Millisecond)
			for _, s := range tt.stages {
				for i := 0; i < s.calls; i++ {
					failure := s.failureFunc(i)
					e.Observe(failure)
					// t.Logf("%s: sample %d: failure %v: successRate %f => %v", tt.name, i, failure, e.successRate.Load(), e.State())
				}
			}
			assert.Equal(t, tt.wantState, e.State())
		})
	}
}
