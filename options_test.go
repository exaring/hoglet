package hoglet

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithHalfOpenDelay(t *testing.T) {
	for name, b := range map[string]Breaker{
		"ewma":          NewEWMABreaker(10, 0.1),
		"slidingWindow": NewSlidingWindowBreaker(time.Second, 0.1),
	} {
		t.Run(name, func(t *testing.T) {
			halfOpenDelay := 500 * time.Millisecond
			sentinelErr := errors.New("foo")
			cb, err := NewCircuit(func(_ context.Context, in error) (any, error) { return nil, in }, b, WithHalfOpenDelay(halfOpenDelay))
			require.NoError(t, err)

			_, err = cb.Call(context.Background(), sentinelErr)
			require.ErrorIs(t, err, sentinelErr)

			_, err = cb.Call(context.Background(), nil)
			assert.Error(t, err, "expected circuit breaker to be open, but it's not")

			time.Sleep(halfOpenDelay)

			_, err = cb.Call(context.Background(), nil)
			assert.NoError(t, err, "expected circuit breaker to be closed again, but it's not")
		})
	}
}
