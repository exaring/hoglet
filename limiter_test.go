package hoglet_test

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/exaring/hoglet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockPanickingObservable struct{}

func (mo mockPanickingObservable) Observe(shouldPanic bool) {
	// abuse the observer interface to signal a panic
	if shouldPanic {
		panic("mockObservable meant to panic")
	}
}

type mockObserverFactory struct{}

func (mof mockObserverFactory) ObserverForCall(_ context.Context, state hoglet.State) (hoglet.Observer, error) {
	// abuse the state argument to control the result of the call, standing in for a [hoglet.Circuit] rejecting calls
	// while open, or after another call claimed the half-open call first
	if state != hoglet.StateClosed {
		return nil, hoglet.ErrCircuitOpen
	}
	return &mockPanickingObservable{}, nil
}

func Test_ConcurrencyLimiter(t *testing.T) {
	type args struct {
		limit int64
		block bool
	}
	tests := []struct {
		name        string
		args        args
		calls       int
		cancel      bool
		wantPanicOn *int // which call to panic on (if at all)
		wantErr     error
	}{
		{
			name:    "under limit",
			args:    args{limit: 1, block: false},
			calls:   0,
			wantErr: nil,
		},
		{
			name:    "over limit; non-blocking",
			args:    args{limit: 1, block: false},
			calls:   1,
			wantErr: hoglet.ErrConcurrencyLimitReached,
		},
		{
			name:    "on limit; blocking",
			args:    args{limit: 1, block: true},
			calls:   1,
			cancel:  true, // cancel simulates a timeout in this case
			wantErr: hoglet.ErrWaitingForSlot,
		},
		{
			name:    "cancellation releases with error",
			args:    args{limit: 1, block: true},
			calls:   1,
			cancel:  true,
			wantErr: context.Canceled,
		},
		{
			name:        "panic releases",
			args:        args{limit: 1, block: true},
			calls:       1,
			cancel:      false,
			wantPanicOn: ptr(0),
			wantErr:     nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctxCalls, cancelCalls := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancelCalls()

			wgStart := &sync.WaitGroup{}
			wgStop := &sync.WaitGroup{}
			defer wgStop.Wait()

			cl := hoglet.ConcurrencyLimiter(tt.args.limit, tt.args.block)
			of, err := cl.Wrap(mockObserverFactory{})
			require.NoError(t, err)
			for i := 0; i < tt.calls; i++ {
				wantPanic := tt.wantPanicOn != nil && *tt.wantPanicOn == i

				f := func() {
					defer wgStop.Done()
					o, err := of.ObserverForCall(ctxCalls, hoglet.StateClosed)
					wgStart.Done()
					require.NoError(t, err)

					<-ctxCalls.Done()

					o.Observe(wantPanic)
				}

				wgStart.Add(1)
				wgStop.Add(1)
				if wantPanic {
					go assert.Panics(t, f)
				} else {
					go f()
				}
			}

			ctx, cancel := context.WithCancel(context.Background())

			if tt.cancel {
				cancel()
			} else {
				defer cancel()
			}

			wgStart.Wait() // ensure all calls are started

			o, err := of.ObserverForCall(ctx, hoglet.StateClosed)
			assert.ErrorIs(t, err, tt.wantErr)
			if tt.wantErr == nil {
				assert.NotNil(t, o)
			}
		})
	}
}

// Test_ConcurrencyLimiter_ReleasesOnInnerError ensures the limiter releases its permit when the inner factory rejects
// the call. A leak there is terminal: every call the circuit rejects after it got a slot (e.g. by losing the race for
// the half-open call) would drain the permits until nothing can reach the circuit to ever close it again.
func Test_ConcurrencyLimiter_ReleasesOnInnerError(t *testing.T) {
	tests := []struct {
		name  string
		block bool
	}{
		{name: "non-blocking", block: false},
		{name: "blocking", block: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const limit = 2

				of, err := hoglet.ConcurrencyLimiter(limit, tt.block).Wrap(mockObserverFactory{})
				require.NoError(t, err)

				// Every acquisition is bounded, so the blocking variant fails instead of hanging on a leaked permit.
				// Costs no wall-clock time under synctest.
				call := func(state hoglet.State) (hoglet.Observer, error) {
					ctx, cancel := context.WithTimeout(t.Context(), time.Second)
					defer cancel()

					return of.ObserverForCall(ctx, state)
				}

				// One more dropped call than there are permits: had they leaked, the last one would be rejected by the
				// limiter instead of the circuit.
				for i := range limit + 1 {
					o, err := call(hoglet.StateHalfOpen)
					require.ErrorIs(t, err, hoglet.ErrCircuitOpen, "call %d", i)
					assert.Nil(t, o) // nothing is handed back that could release the permit for us
				}

				// The circuit closes again: every permit must still be available.
				for i := range limit {
					_, err := call(hoglet.StateClosed)
					require.NoError(t, err, "call %d after recovery: permit leaked while circuit was open", i)
				}

				// The limit is still enforced, i.e. we did not release more than we held.
				_, err = call(hoglet.StateClosed)
				assert.Error(t, err, "limit should be reached with all %d permits held", limit)
			})
		})
	}
}

// Test_ConcurrencyLimiter_OpenCircuitTakesNoSlot ensures calls into an open circuit are rejected by the circuit without
// waiting for or taking a slot, even when all slots are taken.
func Test_ConcurrencyLimiter_OpenCircuitTakesNoSlot(t *testing.T) {
	tests := []struct {
		name  string
		block bool
	}{
		{name: "non-blocking", block: false},
		{name: "blocking", block: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				of, err := hoglet.ConcurrencyLimiter(1, tt.block).Wrap(mockObserverFactory{})
				require.NoError(t, err)

				_, err = of.ObserverForCall(t.Context(), hoglet.StateClosed) // takes the only slot
				require.NoError(t, err)

				// Bounded, so the blocking variant fails instead of hanging if it waits for the slot.
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()

				_, err = of.ObserverForCall(ctx, hoglet.StateOpen)
				assert.ErrorIs(t, err, hoglet.ErrCircuitOpen)
			})
		})
	}
}

func ptr[T any](in T) *T {
	return &in
}
