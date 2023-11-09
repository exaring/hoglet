package hogprom

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/exaring/hoglet"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	namespace = "hoglet"
)

// WithPrometheusMetrics returns a [hoglet.BreakerMiddleware] that registers prometheus metrics for the circuit.
//
// ⚠️ Note: the provided name must be unique across all hoglet instances using the same registerer.
func WithPrometheusMetrics(circuitName string, reg prometheus.Registerer) hoglet.BreakerMiddleware {
	return func(next hoglet.ObserverFactory) (hoglet.ObserverFactory, error) {
		callDurations := prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Namespace: namespace,
				Subsystem: "circuit",
				Name:      "call_durations_seconds",
				Help:      "Call durations in seconds",
				ConstLabels: prometheus.Labels{
					"circuit": circuitName,
				},
			},
			[]string{"success"},
		)

		droppedCalls := prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace: namespace,
				Subsystem: "circuit",
				Name:      "dropped_calls_total",
				Help:      "Total number of calls with an open circuit (i.e.: calls that did not reach the wrapped function)",
				ConstLabels: prometheus.Labels{
					"circuit": circuitName,
				},
			},
			[]string{"cause"},
		)

		inflightCalls := prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: namespace,
				Subsystem: "circuit",
				Name:      "inflight_calls_current",
				Help:      "Current number of calls in-flight",
				ConstLabels: prometheus.Labels{
					"circuit": circuitName,
				},
			},
		)

		for _, c := range []prometheus.Collector{
			callDurations,
			droppedCalls,
			inflightCalls,
		} {
			if err := reg.Register(c); err != nil {
				return nil, fmt.Errorf("hoglet: registering collector: %w", err)
			}
		}

		return &prometheusObserverFactory{
			next: next,

			timesource: wallclock{},

			callDurations: callDurations,
			droppedCalls:  droppedCalls,
			inflightCalls: inflightCalls,
		}, nil
	}
}

type prometheusObserverFactory struct {
	next hoglet.ObserverFactory

	timesource timesource

	callDurations *prometheus.HistogramVec
	droppedCalls  *prometheus.CounterVec
	inflightCalls prometheus.Gauge
}

func (pos *prometheusObserverFactory) ObserverForCall(ctx context.Context, state hoglet.State) (hoglet.Observer, error) {
	o, err := pos.next.ObserverForCall(ctx, state)
	if err != nil {
		pos.droppedCalls.WithLabelValues(errToCause(err)).Inc()
		return nil, err
	}
	start := pos.timesource.Now()
	pos.inflightCalls.Inc()
	return hoglet.ObserverFunc(func(b bool) {
		// invert failure → success to make the metric more intuitive
		pos.callDurations.WithLabelValues(strconv.FormatBool(!b)).Observe(pos.timesource.Since(start).Seconds())
		pos.inflightCalls.Dec()
		o.Observe(b)
	}), nil
}

// errToCause converts known circuit errors to metric labels.
func errToCause(err error) string {
	switch err {
	case hoglet.ErrCircuitOpen:
		return "circuit_open"
	case hoglet.ErrConcurrencyLimitReached:
		return "concurrency_limit"
	default:
		// leave the errors.Is check as last, since it carries a performance penalty
		if errors.Is(err, context.Canceled) {
			return "context_canceled"
		} else if errors.Is(err, context.DeadlineExceeded) {
			return "deadline_exceeded"
		}
		return "other"
	}
}

type timesource interface {
	Now() time.Time
	Since(time.Time) time.Duration
}

// wallclock wraps time.Now/time.Since to allow mocking
type wallclock struct{}

func (wallclock) Now() time.Time {
	return time.Now()
}

func (wallclock) Since(t time.Time) time.Duration {
	return time.Since(t)
}
