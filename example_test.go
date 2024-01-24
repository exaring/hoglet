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
	if bar > 10 {
		return Foo{}, fmt.Errorf("bar is too high!")
	}
	return Foo{Bar: bar}, nil
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

	f, err := h.Call(context.Background(), 1)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(f.Bar)

	_, err = h.Call(context.Background(), 100)
	fmt.Println(err)

	_, err = h.Call(context.Background(), 2)
	fmt.Println(err)

	time.Sleep(time.Second) // wait for half-open delay

	f, err = h.Call(context.Background(), 3)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(f.Bar)

	// Output:
	// 1
	// bar is too high!
	// hoglet: breaker is open
	// 3
}

func ExampleSlidingWindowBreaker() {
	h, err := hoglet.NewCircuit(
		foo,
		hoglet.NewSlidingWindowBreaker(time.Second, 0.1),
	)
	if err != nil {
		log.Fatal(err)
	}

	f, err := h.Call(context.Background(), 1)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(f.Bar)

	_, err = h.Call(context.Background(), 100)
	fmt.Println(err)

	_, err = h.Call(context.Background(), 2)
	fmt.Println(err)

	time.Sleep(time.Second) // wait for sliding window

	f, err = h.Call(context.Background(), 3)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(f.Bar)

	// Output:
	// 1
	// bar is too high!
	// hoglet: breaker is open
	// 3
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
