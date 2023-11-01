package hoglet_test

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/exaring/hoglet"
)

type Foo struct {
	Bar int
}

func foo(ctx context.Context, bar int) (Foo, error) {
	if bar == 42 {
		return Foo{Bar: bar}, nil
	}
	return Foo{}, fmt.Errorf("bar is not 42")
}

func ExampleEWMABreaker() {
	h, err := hoglet.NewCircuit(
		foo,
		hoglet.NewEWMABreaker(10, 0.1),
		hoglet.WithHalfOpenDelay(time.Second),
	)
	if err != nil {
		log.Fatal(err)
	}

	f, err := h.Call(context.Background(), 42)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(f.Bar)

	_, err = h.Call(context.Background(), 0)
	fmt.Println(err)

	_, err = h.Call(context.Background(), 42)
	fmt.Println(err)

	// Output:
	// 42
	// bar is not 42
	// hoglet: breaker is open
}

func ExampleSlidingWindowBreaker() {
	h, err := hoglet.NewCircuit(
		foo,
		hoglet.NewSlidingWindowBreaker(10, 0.1),
	)
	if err != nil {
		log.Fatal(err)
	}

	f, err := h.Call(context.Background(), 42)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(f.Bar)

	_, err = h.Call(context.Background(), 0)
	fmt.Println(err)

	_, err = h.Call(context.Background(), 42)
	fmt.Println(err)

	// Output:
	// 42
	// bar is not 42
	// hoglet: breaker is open
}

func ExampleConcurrencyLimiter() {
	h, err := hoglet.NewCircuit(
		func(ctx context.Context, _ any) (any, error) {
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
			return nil, nil
		},
		hoglet.NewSlidingWindowBreaker(10, 0.1),
		hoglet.WithBreakerMiddleware(hoglet.ConcurrencyLimiter(1, false)),
	)
	if err != nil {
		log.Fatal(err)
	}

	errCh := make(chan error)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() {
		// use up the concurrency limit
		_, _ = h.Call(ctx, 42)
	}()

	// ensure call above actually started
	time.Sleep(time.Millisecond * 100)

	go func() {
		defer close(errCh)
		_, err := h.Call(ctx, 42)
		if err != nil {
			errCh <- err
		}
	}()

	for err := range errCh {
		fmt.Println(err)
	}

	// Output:
	// hoglet: concurrency limit reached
}
