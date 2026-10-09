package hoglet

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errSentinel = errors.New("sentinel error")

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
		return struct{}{}, errSentinel
	default: // noopInPanic
		panic("boom")
	}
}

func BenchmarkHoglet_Do_EWMA(b *testing.B) {
	noop := func(context.Context, struct{}) (out struct{}, err error) { return }
	h, err := NewCircuit(
		NewEWMABreaker(10, 0.9),
		WithHalfOpenDelay(time.Second),
		// WithBreakerMiddleware(ConcurrencyLimiter(1, true)),
	)
	require.NoError(b, err)

	ctx := context.Background() // b.Context() introduces some overhead

	b.ReportAllocs()
	b.ResetTimer()

	f := Wrap(h, noop)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = f(ctx, struct{}{})
		}
	})
}

func BenchmarkHoglet_Do_EWMA_cancellable(b *testing.B) {
	// Each goroutine calls with its own cancellable context, like concurrent requests each carrying theirs, so the
	// circuit watches it for cancellation.
	noop := func(context.Context, struct{}) (out struct{}, err error) { return }
	h, err := NewCircuit(
		NewEWMABreaker(10, 0.9),
		WithHalfOpenDelay(time.Second),
	)
	require.NoError(b, err)

	b.ReportAllocs()
	b.ResetTimer()

	f := Wrap(h, noop)
	b.RunParallel(func(pb *testing.PB) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		for pb.Next() {
			_, _ = f(ctx, struct{}{})
		}
	})
}

func BenchmarkHoglet_Do_SlidingWindow(b *testing.B) {
	noop := func(context.Context, struct{}) (out struct{}, err error) { return }

	h, err := NewCircuit(
		NewSlidingWindowBreaker(10*time.Second, 0.9),
		// WithBreakerMiddleware(ConcurrencyLimiter(1, true)),
	)
	require.NoError(b, err)

	ctx := context.Background() // b.Context() introduces some overhead

	b.ReportAllocs()
	b.ResetTimer()

	f := Wrap(h, noop)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = f(ctx, struct{}{})
		}
	})
}

func BenchmarkHoglet_Do_SlidingWindow_rotating(b *testing.B) {
	// Unlike the 10s window above, this one rotates thousands of times per run, so rotation costs are included.
	const windowSize = time.Millisecond
	noop := func(context.Context, struct{}) (out struct{}, err error) { return }

	h, err := NewCircuit(NewSlidingWindowBreaker(windowSize, 0.9))
	require.NoError(b, err)

	ctx := context.Background() // b.Context() introduces some overhead

	b.ReportAllocs()
	b.ResetTimer()

	f := Wrap(h, noop)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, _ = f(ctx, struct{}{})
		}
	})
	b.ReportMetric(float64(b.Elapsed()/windowSize), "windows")
}

func TestBreaker_nil_breaker_does_not_open(t *testing.T) {
	b, err := NewCircuit(nil)
	require.NoError(t, err)
	_, err = Wrap(b, noop)(t.Context(), noopInFailure)
	assert.Equal(t, errSentinel, err)
	_, err = Wrap(b, noop)(t.Context(), noopInFailure)
	assert.Equal(t, errSentinel, err)
}

func TestBreaker_ctx_parameter_not_cancelled(t *testing.T) {
	noop := func(ctx context.Context, _ any) (context.Context, error) { return ctx, nil }
	b, err := NewCircuit(nil)
	require.NoError(t, err)
	ctx, err := Wrap(b, noop)(t.Context(), noopInSuccess)

	require.NoError(t, err)
	assert.NoError(t, ctx.Err())
}

func TestCircuit_ignored_context_cancellation_still_returned(t *testing.T) {
	noop := func(ctx context.Context, _ any) (string, error) {
		return "expected", ctx.Err()
	}

	b, err := NewCircuit(
		nil,
		WithFailureCondition(IgnoreContextCanceled))
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	out, err := Wrap(b, noop)(ctx, nil)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, "expected", out)
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

func (mt *mockBreaker) apply(o *options) error {
	return nil
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
				{arg: noopInFailure, wantErr: errSentinel},
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
				{arg: noopInFailure, wantErr: errSentinel},
				{arg: noopInSuccess, wantErr: nil, halfOpen: true},
				{arg: noopInSuccess, wantErr: nil},
			},
		},
		{
			name: "failure on half-open keeps open",
			calls: []calls{
				{arg: noopInSuccess, wantErr: nil},
				{arg: noopInFailure, wantErr: errSentinel},
				{arg: noopInFailure, wantErr: errSentinel, halfOpen: true},
				{arg: noopInSuccess, wantErr: ErrCircuitOpen},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			mt := &mockBreaker{}
			h, err := NewCircuit(mt, WithHalfOpenDelay(time.Minute))
			require.NoError(t, err)
			for i, call := range tt.calls {
				if call.halfOpen {
					// simulate passage of time: mark the circuit as opened halfOpenDelay ago
					h.openedAt.Store(nowNanos() - int64(h.halfOpenDelay))
				}

				var err error
				maybeAssertPanic(t, func() {
					_, err = Wrap(h, noop)(t.Context(), call.arg)
				}, call.wantPanic)
				assert.Equal(t, call.wantErr, err, "unexpected error on call %d: %v", i, err)
			}
		})
	}
}

func TestCircuit_only_successful_half_open_call_closes(t *testing.T) {
	tests := []struct {
		name    string
		breaker func() Breaker
	}{
		{name: "ewma", breaker: func() Breaker { return NewEWMABreaker(10, 0.9) }},
		{name: "slidingwindow", breaker: func() Breaker { return NewSlidingWindowBreaker(time.Minute, 0.5) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, err := NewCircuit(tt.breaker(), WithHalfOpenDelay(time.Second))
				require.NoError(t, err)

				// two calls admitted while closed, which only succeed after the circuit opened
				release := make(chan struct{})
				done := make(chan error)
				for range 2 {
					go func() {
						_, err := Wrap(c, func(ctx context.Context, in noopIn) (struct{}, error) {
							<-release
							return noop(ctx, in)
						})(context.Background(), noopInSuccess)
						done <- err
					}()
				}
				synctest.Wait()

				_, err = Wrap(c, noop)(context.Background(), noopInFailure)
				require.ErrorIs(t, err, errSentinel)
				require.Equal(t, StateOpen, c.State())

				close(release)
				require.NoError(t, <-done)
				require.NoError(t, <-done)
				assert.Equal(t, StateOpen, c.State(), "calls admitted before the circuit opened must not close it")

				// their successes lowered the failure rate enough for a failing half-open call to stay below the threshold
				time.Sleep(c.halfOpenDelay)
				_, err = Wrap(c, noop)(context.Background(), noopInFailure)
				require.ErrorIs(t, err, errSentinel)
				assert.Equal(t, StateOpen, c.State(), "a failed half-open call must not close the circuit")

				time.Sleep(c.halfOpenDelay)
				_, err = Wrap(c, noop)(context.Background(), noopInSuccess)
				require.NoError(t, err)
				assert.Equal(t, StateClosed, c.State(), "a successful half-open call must close the circuit")
			})
		})
	}
}

func TestCircuit_ignored_context_error_does_not_mask_wrapped_function_result(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, err := NewCircuit(&mockBreaker{}, WithHalfOpenDelay(time.Minute), WithFailureCondition(IgnoreContextCanceled))
		require.NoError(t, err)

		f := Wrap(c, func(ctx context.Context, in noopIn) (struct{}, error) {
			synctest.Wait() // let the watchdog react to the cancellation before returning
			return noop(ctx, in)
		})

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err = f(ctx, noopInFailure)
		assert.ErrorIs(t, err, errSentinel)
		assert.Equal(t, StateOpen, c.State(), "the wrapped function's failure must have been observed")
	})
}

func TestCircuit_context_error_is_observed_before_wrapped_function_returns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, err := NewCircuit(&mockBreaker{}, WithHalfOpenDelay(time.Minute))
		require.NoError(t, err)

		release := make(chan struct{})
		f := Wrap(c, func(ctx context.Context, in noopIn) (struct{}, error) {
			<-release // ignores its context, like a blocking call would
			return noop(ctx, in)
		})

		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			cancel()
			synctest.Wait() // let the watchdog react to the cancellation
			assert.Equal(t, StateOpen, c.State(), "cancellation must be observed while the wrapped function still runs")
			close(release)
		}()

		_, err = f(ctx, noopInSuccess)
		require.NoError(t, err)
		assert.Equal(t, StateOpen, c.State(), "the call's own success must not override the observed cancellation")
	})
}

func TestCircuit_watchdog_stops_when_the_call_returns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var consulted atomic.Int64
		c, err := NewCircuit(&mockBreaker{}, WithHalfOpenDelay(time.Minute), WithFailureCondition(func(error) bool {
			consulted.Add(1)
			return true
		}))
		require.NoError(t, err)

		// e.g. a server cancels each request's context once its handler returns
		ctx, cancel := context.WithCancel(context.Background())
		_, err = Wrap(c, noop)(ctx, noopInSuccess)
		require.NoError(t, err)

		cancel()
		synctest.Wait() // give a still registered watchdog the chance to fire
		// A watchdog left registered would keep piling up on long-lived contexts until they end.
		assert.Zero(t, consulted.Load(), "the watchdog must be unregistered once the call returned")
		assert.Equal(t, StateClosed, c.State())
	})
}

func TestCircuit_failure_condition_never_called_with_nil_error(t *testing.T) {
	conditionCalled := false
	condition := func(err error) bool {
		conditionCalled = true
		require.NotNil(t, err, "failure condition must not be called with nil error")
		return true
	}

	b, err := NewCircuit(nil, WithFailureCondition(condition))
	require.NoError(t, err)

	_, err = Wrap(b, noop)(t.Context(), noopInSuccess)
	require.NoError(t, err)
	assert.False(t, conditionCalled, "failure condition should not have been called for a successful call")
}

// maybeAssertPanic is a test-table helper to assert that a function panics or not, depending on the value of wantPanic.
func maybeAssertPanic(t *testing.T, f func(), wantPanic any) {
	wrapped := assert.NotPanics
	if wantPanic != nil {
		wrapped = func(t assert.TestingT, f assert.PanicTestFunc, msgAndArgs ...any) bool {
			return assert.PanicsWithValue(t, wantPanic, f, msgAndArgs...)
		}
	}
	wrapped(t, f)
}

func TestCircuit_limiter_rejection_does_not_use_up_half_open_call(t *testing.T) {
	c, err := NewCircuit(&mockBreaker{}, WithHalfOpenDelay(time.Minute), WithBreakerMiddleware(ConcurrencyLimiter(1, false)))
	require.NoError(t, err)

	// take the only slot with a call admitted while closed
	inflight, err := c.observerFactory.ObserverForCall(context.Background(), c.State())
	require.NoError(t, err)

	// simulate passage of time: mark the circuit as opened halfOpenDelay ago
	c.openedAt.Store(nowNanos() - int64(c.halfOpenDelay))

	_, err = Wrap(c, noop)(context.Background(), noopInSuccess)
	require.ErrorIs(t, err, ErrConcurrencyLimitReached)
	assert.Equal(t, StateHalfOpen, c.State(), "a call rejected by the limiter must not use up the half-open call")

	inflight.Observe(true) // frees the slot; a failure cannot close the circuit

	_, err = Wrap(c, noop)(context.Background(), noopInSuccess)
	require.NoError(t, err, "the next call must get the half-open call")
	assert.Equal(t, StateClosed, c.State())
}

func TestCircuit_call_waiting_for_slot_does_not_enter_circuit_opened_meanwhile(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, err := NewCircuit(&mockBreaker{}, WithHalfOpenDelay(time.Minute), WithBreakerMiddleware(ConcurrencyLimiter(1, true)))
		require.NoError(t, err)

		// take the only slot with a call admitted while closed
		inflight, err := c.observerFactory.ObserverForCall(context.Background(), c.State())
		require.NoError(t, err)

		errCh := make(chan error)
		go func() {
			_, err := Wrap(c, noop)(context.Background(), noopInSuccess)
			errCh <- err
		}()
		synctest.Wait() // the call saw the circuit closed and now waits for the slot

		inflight.Observe(true) // opens the circuit, then frees the slot

		assert.ErrorIs(t, <-errCh, ErrCircuitOpen)
	})
}
