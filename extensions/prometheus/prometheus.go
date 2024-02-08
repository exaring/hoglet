package hogprom

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/exaring/hoglet"
	"github.com/prometheus/client_golang/prometheus"
)

const (
	namespace = "hoglet"
	subsystem = "circuit"
)

// NewCollector returns a [hoglet.BreakerMiddleware] that exposes prometheus metrics for the circuit.
// It implements prometheus.Collector and can therefore be registered with a prometheus.Registerer.
//
// ⚠️ Note: the provided name must be unique across all hoglet instances ultimately registered to the same
// prometheus.Registerer.
func NewCollector(circuitName string) *Middleware {
	callDurations := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: namespace,
			Subsystem: subsystem,
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
			Subsystem: subsystem,
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
			Subsystem: subsystem,
			Name:      "inflight_calls_current",
			Help:      "Current number of calls in-flight",
			ConstLabels: prometheus.Labels{
				"circuit": circuitName,
			},
		},
	)

	return &Middleware{
		callDurations: callDurations,
		droppedCalls:  droppedCalls,
		inflightCalls: inflightCalls,
	}
}

type Middleware struct {
	callDurations *prometheus.HistogramVec
	droppedCalls  *prometheus.CounterVec
	inflightCalls prometheus.Gauge
}

func (m Middleware) Collect(ch chan<- prometheus.Metric) {
	m.callDurations.Collect(ch)
	m.droppedCalls.Collect(ch)
	m.inflightCalls.Collect(ch)
}

func (m Middleware) Describe(ch chan<- *prometheus.Desc) {
	prometheus.DescribeByCollect(m, ch)
}

func (m Middleware) Wrap(of hoglet.ObserverFactory) (hoglet.ObserverFactory, error) {
	return &wrappedMiddleware{
		Middleware: m,
		next:       of,
		timesource: wallclock{},
	}, nil
}

type wrappedMiddleware struct {
	Middleware
	next       hoglet.ObserverFactory
	timesource timesource
}

func (wm *wrappedMiddleware) ObserverForCall(ctx context.Context, state hoglet.State) (hoglet.Observer, error) {
	o, err := wm.next.ObserverForCall(ctx, state)
	if err != nil {
		wm.droppedCalls.WithLabelValues(errToCause(err)).Inc()
		return nil, err
	}
	start := wm.timesource.Now()
	wm.inflightCalls.Inc()
	return hoglet.ObserverFunc(func(b bool) {
		// invert failure → success to make the metric more intuitive
		wm.callDurations.WithLabelValues(strconv.FormatBool(!b)).Observe(wm.timesource.Since(start).Seconds())
		wm.inflightCalls.Dec()
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
