package hoglet

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func Test_newLimiter(t *testing.T) {
	orig := func(context.Context) (observer, error) {
		return &mockObservable{}, nil
	}

	type args struct {
		limit int64
		block bool
	}
	tests := []struct {
		name    string
		args    args
		calls   int
		cancel  bool
		wantErr error
	}{
		{"under limit", args{limit: 1, block: false}, 0, false, nil},
		{"over limit; non-blocking", args{limit: 1, block: false}, 2, false, ErrConcurrencyLimitReached},
		{"over limit; blocking", args{limit: 1, block: true}, 2, false, ErrWaitingForSlot},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()

			wg := &sync.WaitGroup{}

			of := newLimiter(orig, tt.args.limit, tt.args.block)
			for i := 0; i < tt.calls; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, _ = of(ctx)
				}()
			}

			wg.Wait()

			if tt.cancel {
				cancel()
			}

			_, err := of(ctx)
			assert.ErrorIs(t, err, tt.wantErr)
		})
	}
}
