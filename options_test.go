package hoglet_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/exaring/hoglet"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithHalfOpenDelay(t *testing.T) {
	noop := func(_ context.Context, in error) (any, error) { return nil, in }

	for name, b := range map[string]hoglet.Breaker{
		"ewma":          hoglet.NewEWMABreaker(10, 0.1),
		"slidingWindow": hoglet.NewSlidingWindowBreaker(time.Second, 0.1),
	} {
		t.Run(name, func(t *testing.T) {
			halfOpenDelay := 500 * time.Millisecond
			sentinelErr := errors.New("foo")
			cb, err := hoglet.NewCircuit(b, hoglet.WithHalfOpenDelay(halfOpenDelay))
			require.NoError(t, err)

			_, err = hoglet.Wrap(cb, noop)(context.Background(), sentinelErr)
			require.ErrorIs(t, err, sentinelErr)

			_, err = hoglet.Wrap(cb, noop)(context.Background(), nil)
			assert.Error(t, err, "expected circuit breaker to be open, but it's not")

			time.Sleep(halfOpenDelay)

			_, err = hoglet.Wrap(cb, noop)(context.Background(), nil)
			assert.NoError(t, err, "expected circuit breaker to be closed again, but it's not")
		})
	}
}
