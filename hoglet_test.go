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

func BenchmarkHoglet_Do_EWMA(b *testing.B) {
	breaker := hoglet.NewCircuit(
		func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
		hoglet.NewEWMABreaker(10, 0.9, 2*time.Second),
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

func BenchmarkHoglet_Do_SlidingWindow(b *testing.B) {
	breaker := hoglet.NewCircuit(
		func(context.Context, struct{}) (struct{}, error) { return struct{}{}, nil },
		hoglet.NewSlidingWindowBreaker(10*time.Second, 0.9, 2*time.Second),
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
	b := &hoglet.Circuit[struct{}, struct{}]{}
	_, err := b.Do(context.Background(), struct{}{})
	assert.NoError(t, err)
}

func TestBreaker_nil_breaker_does_not_open(t *testing.T) {
	b := hoglet.NewCircuit(noop, nil)
	_, err := b.Do(context.Background(), noopInFailure)
	assert.Equal(t, sentinel, err)
	_, err = b.Do(context.Background(), noopInFailure)
	assert.Equal(t, sentinel, err)
}

// mockBreaker is a mock implementation of the [Breaker] interface that opens or closes depending on the last observed
// failure.
type mockBreaker struct {
	open bool
}

// Observe implements hoglet.Breaker
func (mt *mockBreaker) Call() hoglet.Observable {
	if mt.open {
		return nil
	} else {
		return &mockObservable{breaker: mt}
	}
}

type mockObservable struct {
	breaker *mockBreaker
}

func (mo *mockObservable) Observe(failure bool) {
	mo.breaker.open = failure
}

func TestHoglet_Do(t *testing.T) {
	type calls struct {
		arg       noopIn
		halfOpen  bool // put the breaker in the half-open state BEFORE calling
		wantErr   error
		wantPanic any
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
				{arg: noopInSuccess, wantErr: hoglet.ErrCircuitOpen},
			},
		},
		{
			name: "panic opens",
			calls: []calls{
				{arg: noopInSuccess, wantErr: nil},
				{arg: noopInPanic, wantErr: nil, wantPanic: "boom"},
				{arg: noopInSuccess, wantErr: hoglet.ErrCircuitOpen},
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
				{arg: noopInSuccess, wantErr: hoglet.ErrCircuitOpen},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mt := &mockBreaker{}
			h := hoglet.NewCircuit(noop, mt)
			for i, c := range tt.calls {
				if c.halfOpen {
					mt.open = false
				}

				var err error
				maybeAssertPanic := assert.NotPanics
				if c.wantPanic != nil {
					maybeAssertPanic = func(t assert.TestingT, f assert.PanicTestFunc, msgAndArgs ...interface{}) bool {
						return assert.PanicsWithValue(t, c.wantPanic, f, msgAndArgs...)
					}
				}
				maybeAssertPanic(t, func() {
					_, err = h.Do(context.Background(), c.arg)
				})
				assert.Equal(t, c.wantErr, err, "unexpected error on call %d: %v", i, err)
			}
		})
	}
}
