package hoglet_test

import (
	"context"
	"sync"
	"testing"
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

func (mof mockObserverFactory) ObserverForCall(ctx context.Context, state hoglet.State) (hoglet.Observer, error) {
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
			name:    "cancelation releases with error",
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
			of, err := cl(mockObserverFactory{})
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

func ptr[T any](in T) *T {
	return &in
}
