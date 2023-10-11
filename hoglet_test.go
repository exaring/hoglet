package hoglet

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

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
	breaker := NewCircuit(
		func(context.Context, struct{}) (out struct{}, err error) { return },
		NewEWMABreaker(10, 0.9),
	)

	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = breaker.Call(ctx, struct{}{})
		}
	})
}

func BenchmarkHoglet_Do_SlidingWindow(b *testing.B) {
	breaker := NewCircuit(
		func(context.Context, struct{}) (out struct{}, err error) { return },
		NewSlidingWindowBreaker(10*time.Second, 0.9),
	)

	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = breaker.Call(ctx, struct{}{})
		}
	})
}

func TestBreaker_nil_breaker_does_not_open(t *testing.T) {
	b := NewCircuit(noop, nil)
	_, err := b.Call(context.Background(), noopInFailure)
	assert.Equal(t, sentinel, err)
	_, err = b.Call(context.Background(), noopInFailure)
	assert.Equal(t, sentinel, err)
}

// mockBreaker is a mock implementation of the [Breaker] interface that opens or closes depending on the last observed
// failure.
type mockBreaker struct {
	open bool
}

// observerForCall implements [Breaker]
func (mt *mockBreaker) observerForCall() observer {
	if mt.open {
		return nil
	} else {
		return &mockObservable{breaker: mt}
	}
}

// connect implements [Breaker]
func (mt *mockBreaker) connect(untypedCircuit) {}

type mockObservable struct {
	breaker *mockBreaker
	once    sync.Once
}

// observe implements [Observer]
func (mo *mockObservable) observe(failure bool) {
	mo.once.Do(func() {
		mo.breaker.open = failure
	})
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
				{arg: noopInSuccess, wantErr: ErrCircuitOpen},
			},
		},
		{
			name: "panic opens",
			calls: []calls{
				{arg: noopInSuccess, wantErr: nil},
				{arg: noopInPanic, wantErr: nil, wantPanic: "boom"},
				{arg: noopInSuccess, wantErr: ErrCircuitOpen},
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
				{arg: noopInSuccess, wantErr: ErrCircuitOpen},
			},
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mt := &mockBreaker{}
			h := NewCircuit(noop, mt)
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
					_, err = h.Call(context.Background(), c.arg)
				})
				assert.Equal(t, c.wantErr, err, "unexpected error on call %d: %v", i, err)
			}
		})
	}
}
