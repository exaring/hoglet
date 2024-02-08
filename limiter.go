package hoglet

import (
	"context"
	"fmt"

	"golang.org/x/sync/semaphore"
)

// ConcurrencyLimiter is a [BreakerMiddleware] that sets the maximum number of concurrent calls to the provided limit.
// If the limit is reached, the circuit's behavior depends on the blocking parameter:
//   - it either returns [ErrConcurrencyLimitReached] immediately if blocking is false
//   - or blocks until a slot is available if blocking is true, potentially returning [ErrWaitingForSlot]. The returned
//     error wraps the underlying cause (e.g. [context.Canceled] or [context.DeadlineExceeded]).
func ConcurrencyLimiter(limit int64, block bool) BreakerMiddleware {
	return BreakerMiddlewareFunc(func(next ObserverFactory) (ObserverFactory, error) {
		cl := concurrencyLimiter{
			sem:  semaphore.NewWeighted(limit),
			next: next,
		}
		if block {
			return concurrencyLimiterBlocking{
				concurrencyLimiter: cl,
			}, nil
		}
		return concurrencyLimiterNonBlocking{
			concurrencyLimiter: cl,
		}, nil
	})
}

type concurrencyLimiter struct {
	sem  *semaphore.Weighted
	next ObserverFactory
}

func (cl concurrencyLimiter) ObserverForCall(ctx context.Context, state State) (Observer, error) {
	o, err := cl.next.ObserverForCall(ctx, state)
	if err != nil {
		return nil, err
	}
	return ObserverFunc(func(b bool) {
		defer cl.sem.Release(1)
		o.Observe(b)
	}), nil
}

type concurrencyLimiterBlocking struct {
	concurrencyLimiter
}

func (clb concurrencyLimiterBlocking) ObserverForCall(ctx context.Context, state State) (Observer, error) {
	if err := clb.sem.Acquire(ctx, 1); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrWaitingForSlot, err)
	}
	return clb.concurrencyLimiter.ObserverForCall(ctx, state)
}

type concurrencyLimiterNonBlocking struct {
	concurrencyLimiter
}

func (clnb concurrencyLimiterNonBlocking) ObserverForCall(ctx context.Context, state State) (Observer, error) {
	if !clnb.sem.TryAcquire(1) {
		return nil, ErrConcurrencyLimitReached
	}
	return clnb.concurrencyLimiter.ObserverForCall(ctx, state)
}
