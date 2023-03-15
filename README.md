[![Build Status](https://github.com/exaring/hoglet/actions/workflows/main.yaml/badge.svg)](https://github.com/exaring/hoglet/actions/workflows/main.yaml)
[![Go Reference](https://pkg.go.dev/badge/github.com/exaring/hoglet.svg)](https://pkg.go.dev/github.com/exaring/hoglet)
[![Go Report Card](https://goreportcard.com/badge/github.com/exaring/hoglet)](https://goreportcard.com/report/github.com/exaring/hoglet)

# hoglet

Simple low-overhead circuit breaker library.

## Usage

```go
h := hoglet.NewBreaker(
    func(ctx context.Context, bar int) (Foo, error) {
        if bar == 42 {
            return Foo{Bar: bar}, nil
        }
        return Foo{}, fmt.Errorf("bar is not 42")
    },
    hoglet.NewSlidingWindowTrigger(10, 0.1, 5*time.Second),
    hoglet.WithFailureCondition(hoglet.IgnoreContextCancelation),
)
f, _ := h.Do(context.Background(), 42)
fmt.Println(f.Bar) // 42

_, err = h.Do(context.Background(), 0)
fmt.Println(err) // bar is not 42

_, err = h.Do(context.Background(), 42)
fmt.Println(err) // hoglet: breaker is open
```
