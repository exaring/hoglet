package hoglet

import (
	"context"
	"fmt"

	"golang.org/x/sync/semaphore"
)

func newLimiter(origFactory observerFactory, limit int64, block bool) observerFactory {
	sem := semaphore.NewWeighted(limit)

	wrappedFactory := func(ctx context.Context) (observer, error) {
		o, err := origFactory(ctx)
		if err != nil {
			return nil, err
		}
		return observableCall(func(b bool) {
			o.observe(b)
			sem.Release(1)
		}), nil
	}

	if block {
		return func(ctx context.Context) (observer, error) {
			if err := sem.Acquire(ctx, 1); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrWaitingForSlot, err)
			}
			return wrappedFactory(ctx)
		}
	}
	return func(ctx context.Context) (observer, error) {
		if sem.TryAcquire(1) {
			return wrappedFactory(ctx)
		} else {
			return nil, ErrConcurrencyLimitReached
		}
	}
}
