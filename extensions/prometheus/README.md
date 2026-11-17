# hoglet prometheus extension

Package `hogprom` exposes Prometheus metrics for a [hoglet](../../README.md) circuit. The collector is a
`hoglet.BreakerMiddleware` and also a `prometheus.Collector`.

## Install

```sh
go get github.com/exaring/hoglet/extensions/prometheus
```

## Usage

```go
import (
    "github.com/exaring/hoglet"
    hogprom "github.com/exaring/hoglet/extensions/prometheus"
    "github.com/prometheus/client_golang/prometheus"
)

m := hogprom.NewCollector("my-circuit")
prometheus.MustRegister(m)

c, err := hoglet.NewCircuit(
    hoglet.NewSlidingWindowBreaker(5*time.Second, 0.1),
    hoglet.WithBreakerMiddleware(hoglet.ConcurrencyLimiter(10, false)),
    hoglet.WithBreakerMiddleware(m), // after the limiter, so it counts the calls the limiter drops
)
/* if err != nil ... */
```

- The circuit name must be unique for each `prometheus.Registerer`.
- Middlewares wrap from the inside out. Add the collector after the middlewares that it must report on.

## Metrics

All metrics have a `circuit` label with the name that you give to `NewCollector`.

| Metric                                  | Type      | Labels                                                                                         |
|-----------------------------------------|-----------|------------------------------------------------------------------------------------------------|
| `hoglet_circuit_call_durations_seconds` | histogram | `success`: `true`, `false`                                                                     |
| `hoglet_circuit_dropped_calls_total`    | counter   | `cause`: `circuit_open`, `concurrency_limit`, `context_canceled`, `deadline_exceeded`, `other` |
| `hoglet_circuit_inflight_calls_current` | gauge     |                                                                                                |

A dropped call is a call that did not get to the wrapped function.
