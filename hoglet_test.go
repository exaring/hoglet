package hoglet_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/exaring/hoglet"
	"github.com/stretchr/testify/assert"
)

var sentinel = errors.New("sentinel error")

type noopIn int

const (
	noopInSuccess noopIn = iota
	noopInFailure
	noopInPanic
)

// noop is just a simple breakable function for tests.
func noop(ctx context.Context, in noopIn) (struct{}, error) {
	switch in {
	case noopInSuccess:
		return struct{}{}, nil
	case noopInFailure:
		return struct{}{}, sentinel
	default: // noopInPanic
		panic("boom")
	}
}

func BenchmarkHoglet(b *testing.B) {
	breaker := hoglet.NewBreaker(
		func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
		hoglet.NewEWMATrigger(10, 0.9, 2*time.Second),
	)

	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = breaker.Do(ctx, struct{}{})
		}
	})
}

func TestBreaker_zero_value_does_not_panic(t *testing.T) {
	b := &hoglet.Breaker[struct{}, struct{}]{}
	_, err := b.Do(context.Background(), struct{}{})
	assert.NoError(t, err)
}

func TestBreaker_nil_trigger_does_not_open(t *testing.T) {
	b := hoglet.NewBreaker(noop, nil)
	_, err := b.Do(context.Background(), noopInFailure)
	assert.Equal(t, sentinel, err)
	_, err = b.Do(context.Background(), noopInFailure)
	assert.Equal(t, sentinel, err)
}

// mockTrigger is a mock implementation of the Trigger interface that opens or closes depending on the last observed
// failure.
type mockTrigger struct {
	state hoglet.State
}

// Observe implements hoglet.Trigger
func (mt *mockTrigger) Observe(failure bool) {
	if failure {
		mt.state = hoglet.StateOpen
	} else {
		mt.state = hoglet.StateClosed
	}
}

// State implements hoglet.Trigger
func (mt *mockTrigger) State() hoglet.State {
	return mt.state
}

var _ hoglet.Trigger = (*mockTrigger)(nil)

func TestHoglet_Do(t *testing.T) {
	type calls struct {
		arg       noopIn
		halfOpen  bool // put the trigger in the half-open state BEFORE calling
		wantErr   error
		wantPanic bool
	}
	tests := []struct {
		name  string
		calls []calls
	}{
		{
			name: "no errors; always closed",
			calls: []calls{
				{arg: noopInSuccess, wantErr: nil},
				{arg: noopInSuccess, wantErr: nil},
				{arg: noopInSuccess, wantErr: nil},
			},
		},
		{
			name: "error opens",
			calls: []calls{
				{arg: noopInSuccess, wantErr: nil},
				{arg: noopInFailure, wantErr: sentinel},
				{arg: noopInSuccess, wantErr: hoglet.ErrBreakerOpen},
			},
		},
		{
			name: "panic opens",
			calls: []calls{
				{arg: noopInSuccess, wantErr: nil},
				{arg: noopInPanic, wantErr: nil, wantPanic: true},
				{arg: noopInSuccess, wantErr: hoglet.ErrBreakerOpen},
			},
		},
		{
			name: "success on half-open closes",
			calls: []calls{
				{arg: noopInSuccess, wantErr: nil},
				{arg: noopInFailure, wantErr: sentinel},
				{arg: noopInSuccess, wantErr: nil, halfOpen: true},
				{arg: noopInSuccess, wantErr: nil},
			},
		},
		{
			name: "failure on half-open keeps open",
			calls: []calls{
				{arg: noopInSuccess, wantErr: nil},
				{arg: noopInFailure, wantErr: sentinel},
				{arg: noopInFailure, wantErr: sentinel, halfOpen: true},
				{arg: noopInSuccess, wantErr: hoglet.ErrBreakerOpen},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mt := &mockTrigger{state: hoglet.StateClosed}
			h := hoglet.NewBreaker(noop, mt)
			for i, c := range tt.calls {
				if c.halfOpen {
					mt.state = hoglet.StateHalfOpen
				}

				var err error
				panicAssert := assert.NotPanics
				if c.wantPanic {
					panicAssert = assert.Panics
				}
				panicAssert(t, func() {
					_, err = h.Do(context.Background(), c.arg)
				})
				assert.Equal(t, c.wantErr, err, "unexpected error on call %d: %v", i, err)
			}
		})
	}
}
