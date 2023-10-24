package hoglet

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockPanickingObservable struct{}

func (mo *mockPanickingObservable) observe(shouldPanic bool) {
	// abuse the observer interface to signal a panic
	if shouldPanic {
		panic("mockObservable meant to panic")
	}
}

func Test_newLimiter(t *testing.T) {
	orig := func() observerFactory {
		return func(context.Context) (observer, error) {
			return &mockPanickingObservable{}, nil
		}
	}

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
			wantErr: ErrConcurrencyLimitReached,
		},
		{
			name:    "on limit; blocking",
			args:    args{limit: 1, block: true},
			calls:   1,
			cancel:  true, // cancel simulates a timeout in this case
			wantErr: ErrWaitingForSlot,
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

			of := newLimiter(orig(), tt.args.limit, tt.args.block)
			for i := 0; i < tt.calls; i++ {
				wantPanic := tt.wantPanicOn != nil && *tt.wantPanicOn == i

				f := func() {
					defer wgStop.Done()
					o, err := of(ctxCalls)
					wgStart.Done()
					require.NoError(t, err)

					<-ctxCalls.Done()

					o.observe(wantPanic)
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

			o, err := of(ctx)
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
