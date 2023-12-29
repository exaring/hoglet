[![Build Status](https://github.com/exaring/hoglet/actions/workflows/main.yaml/badge.svg)](https://github.com/exaring/hoglet/actions/workflows/main.yaml)
[![Go Reference](https://pkg.go.dev/badge/github.com/exaring/hoglet.svg)](https://pkg.go.dev/github.com/exaring/hoglet)
[![Go Report Card](https://goreportcard.com/badge/github.com/exaring/hoglet)](https://goreportcard.com/report/github.com/exaring/hoglet)

# hoglet

Simple low-overhead circuit breaker library.

## Usage

```go
h, err := hoglet.NewCircuit(
    func(ctx context.Context, bar int) (Foo, error) {
        if bar == 42 {
            return Foo{Bar: bar}, nil
        }
        return Foo{}, fmt.Errorf("bar is not 42")
    },
    hoglet.NewSlidingWindowBreaker(10, 0.1),
    hoglet.WithFailureCondition(hoglet.IgnoreContextCanceled),
)
/* if err != nil ... */

f, _ := h.Call(context.Background(), 42)
fmt.Println(f.Bar) // 42

_, err = h.Call(context.Background(), 0)
fmt.Println(err) // bar is not 42

_, err = h.Call(context.Background(), 42)
fmt.Println(err) // hoglet: breaker is open
```

## Operation

Each call to the wrapped function (via `Circuit.Call`) is tracked and its result "observed". Breakers then react to
these observations according to their own logic, optionally opening the circuit.

An open circuit does not allow any calls to go through, and will return an error immediately.

If the wrapped function blocks, `Circuit.Call` will block as well, but any context cancellations or expirations will
count towards the failure rate, allowing the circuit to respond timely to failures, while still having well-defined and
non-racy behavior around the failed function.


## Design

Hoglet prefers throughput to correctness (e.g. by avoiding locks), which means it cannot guarantee an exact number of
calls will go through.