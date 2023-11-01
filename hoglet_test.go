package hoglet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	h, err := NewCircuit(
		func(context.Context, struct{}) (out struct{}, err error) { return },
		NewEWMABreaker(10, 0.9),
		WithBreakerMiddleware(ConcurrencyLimiter(1, true)),
	)
	require.NoError(b, err)

	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = h.Call(ctx, struct{}{})
		}
	})
}

func BenchmarkHoglet_Do_SlidingWindow(b *testing.B) {
	h, err := NewCircuit(
		func(context.Context, struct{}) (out struct{}, err error) { return },
		NewSlidingWindowBreaker(10*time.Second, 0.9),
		WithBreakerMiddleware(ConcurrencyLimiter(1, true)),
	)
	require.NoError(b, err)

	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = h.Call(ctx, struct{}{})
		}
	})
}

func TestBreaker_nil_breaker_does_not_open(t *testing.T) {
	b, err := NewCircuit(noop, nil)
	require.NoError(t, err)
	_, err = b.Call(context.Background(), noopInFailure)
	assert.Equal(t, sentinel, err)
	_, err = b.Call(context.Background(), noopInFailure)
	assert.Equal(t, sentinel, err)
}

// mockBreaker is a mock implementation of the [Breaker] interface that opens or closes depending on the last observed
// failure.
type mockBreaker struct{}

// observer implements [Breaker]
func (mt *mockBreaker) observe(halfOpen, failure bool) stateChange {
	if failure {
		return stateChangeOpen
	}
	return stateChangeClose
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
			h, err := NewCircuit(noop, mt)
			require.NoError(t, err)
			for i, call := range tt.calls {
				if call.halfOpen {
					h.setOpenedAt(int64(h.halfOpenDelay))
				}

				var err error
				maybeAssertPanic := assert.NotPanics
				if call.wantPanic != nil {
					maybeAssertPanic = func(t assert.TestingT, f assert.PanicTestFunc, msgAndArgs ...interface{}) bool {
						return assert.PanicsWithValue(t, call.wantPanic, f, msgAndArgs...)
					}
				}
				maybeAssertPanic(t, func() {
					_, err = h.Call(context.Background(), call.arg)
				})
				assert.Equal(t, call.wantErr, err, "unexpected error on call %d: %v", i, err)
			}
		})
	}
}
