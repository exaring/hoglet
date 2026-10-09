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
//
// Calls into an open circuit neither wait for nor take a slot: the circuit rejects them right away.
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

// concurrencyLimiter holds the shared state of both limiter variants. Calls into an open circuit skip the limiter. For
// all others, both acquire a permit before delegating to its [concurrencyLimiter.ObserverForCall], which is therefore
// responsible for releasing it again — including when the inner factory rejects the call.
type concurrencyLimiter struct {
	sem  *semaphore.Weighted
	next ObserverFactory
}

func (cl concurrencyLimiter) ObserverForCall(ctx context.Context, state State) (Observer, error) {
	o, err := cl.next.ObserverForCall(ctx, state)
	if err != nil {
		// No [Observer] is returned on error, so release here or never.
		// Leaking a permit here is terminal: every call the circuit rejects after it got a slot
		// (e.g. by losing the race for the half-open call) would permanently shrink the effective
		// limit until no call can reach the circuit to ever close it again.
		cl.sem.Release(1)
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
	if state == StateOpen {
		return clb.next.ObserverForCall(ctx, state) // rejected by the circuit: nothing to limit
	}
	if err := clb.sem.Acquire(ctx, 1); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrWaitingForSlot, err)
	}
	return clb.concurrencyLimiter.ObserverForCall(ctx, state)
}

type concurrencyLimiterNonBlocking struct {
	concurrencyLimiter
}

func (clnb concurrencyLimiterNonBlocking) ObserverForCall(ctx context.Context, state State) (Observer, error) {
	if state == StateOpen {
		return clnb.next.ObserverForCall(ctx, state) // rejected by the circuit: nothing to limit
	}
	if !clnb.sem.TryAcquire(1) {
		return nil, ErrConcurrencyLimitReached
	}
	return clnb.concurrencyLimiter.ObserverForCall(ctx, state)
}
