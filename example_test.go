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

func Example() {
	h := hoglet.NewBreaker(
		foo,
		hoglet.NewEWMATrigger(10, 0.9, 5*time.Second),
		hoglet.WithFailureCondition(hoglet.IgnoreContextCancelation),
	)
	f, err := h.Do(context.Background(), 42)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(f.Bar)

	_, err = h.Do(context.Background(), 0)
	fmt.Println(err)

	_, err = h.Do(context.Background(), 42)
	fmt.Println(err)

	// Output:
	// 42
	// bar is not 42
	// hoglet: breaker is open
}
