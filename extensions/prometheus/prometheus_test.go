package hogprom

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/exaring/hoglet"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
)

type mockObserverFactory struct{}

// ObserverForCall implements hoglet.ObserverFactory.
func (*mockObserverFactory) ObserverForCall(_ context.Context, state hoglet.State) (hoglet.Observer, error) {
	// this simple factory abuses the state argument to directly control the result of the call
	switch state {
	case hoglet.StateClosed:
		return mockObserver{}, nil
	case hoglet.StateOpen:
		return nil, hoglet.ErrCircuitOpen
	default:
		panic("not implemented")
	}
}

type mockObserver struct{}

func (mockObserver) Observe(bool) {}

type mockTimesource struct {
	t time.Time
}

func (m mockTimesource) Now() time.Time {
	return m.t
}

func (m mockTimesource) Since(t time.Time) time.Duration {
	return m.t.Sub(t)
}

func TestWithPrometheusMetrics(t *testing.T) {
	reg := prometheus.NewPedanticRegistry()
	m := WithPrometheusMetrics("test", reg)
	of, err := m(&mockObserverFactory{})
	require.NoError(t, err)

	mt := &mockTimesource{time.Now()}

	of.(*prometheusObserverFactory).timesource = mt

	inflightOut0 := `# HELP hoglet_circuit_inflight_calls_current Current number of calls in-flight
                     # TYPE hoglet_circuit_inflight_calls_current gauge
                     hoglet_circuit_inflight_calls_current{circuit="test"} 0
                    `

	if err := testutil.GatherAndCompare(reg, strings.NewReader(inflightOut0)); err != nil {
		t.Fatal(err)
	}

	_, err = of.ObserverForCall(context.Background(), hoglet.StateOpen)
	require.ErrorIs(t, err, hoglet.ErrCircuitOpen)

	droppedOut1 := `# HELP hoglet_circuit_dropped_calls_total Total number of calls with an open circuit (i.e.: calls that did not reach the wrapped function)
                    # TYPE hoglet_circuit_dropped_calls_total counter
                    hoglet_circuit_dropped_calls_total{cause="circuit_open",circuit="test"} 1
                    # HELP hoglet_circuit_inflight_calls_current Current number of calls in-flight
                    # TYPE hoglet_circuit_inflight_calls_current gauge
                    hoglet_circuit_inflight_calls_current{circuit="test"} 0
				   `
	if err := testutil.GatherAndCompare(reg, strings.NewReader(droppedOut1)); err != nil {
		t.Fatal(err)
	}

	o, err := of.ObserverForCall(context.Background(), hoglet.StateClosed)
	require.NoError(t, err)

	inflightOut1 := `# HELP hoglet_circuit_dropped_calls_total Total number of calls with an open circuit (i.e.: calls that did not reach the wrapped function)
                     # TYPE hoglet_circuit_dropped_calls_total counter
                     hoglet_circuit_dropped_calls_total{cause="circuit_open",circuit="test"} 1
                     # HELP hoglet_circuit_inflight_calls_current Current number of calls in-flight
                     # TYPE hoglet_circuit_inflight_calls_current gauge
                     hoglet_circuit_inflight_calls_current{circuit="test"} 1
				   `
	if err := testutil.GatherAndCompare(reg, strings.NewReader(inflightOut1)); err != nil {
		t.Fatal(err)
	}

	mt.t = mt.t.Add(time.Second) // move the clock 1 second forward

	o.Observe(true)

	durationsOut1 := `# HELP hoglet_circuit_call_durations_seconds Call durations in seconds
	                  # TYPE hoglet_circuit_call_durations_seconds histogram
	                  hoglet_circuit_call_durations_seconds_bucket{circuit="test",success="false",le="0.005"} 0
	                  hoglet_circuit_call_durations_seconds_bucket{circuit="test",success="false",le="0.01"} 0
	                  hoglet_circuit_call_durations_seconds_bucket{circuit="test",success="false",le="0.025"} 0
	                  hoglet_circuit_call_durations_seconds_bucket{circuit="test",success="false",le="0.05"} 0
	                  hoglet_circuit_call_durations_seconds_bucket{circuit="test",success="false",le="0.1"} 0
	                  hoglet_circuit_call_durations_seconds_bucket{circuit="test",success="false",le="0.25"} 0
	                  hoglet_circuit_call_durations_seconds_bucket{circuit="test",success="false",le="0.5"} 0
	                  hoglet_circuit_call_durations_seconds_bucket{circuit="test",success="false",le="1"} 1
	                  hoglet_circuit_call_durations_seconds_bucket{circuit="test",success="false",le="2.5"} 1
	                  hoglet_circuit_call_durations_seconds_bucket{circuit="test",success="false",le="5"} 1
	                  hoglet_circuit_call_durations_seconds_bucket{circuit="test",success="false",le="10"} 1
	                  hoglet_circuit_call_durations_seconds_bucket{circuit="test",success="false",le="+Inf"} 1
	                  hoglet_circuit_call_durations_seconds_sum{circuit="test",success="false"} 1
	                  hoglet_circuit_call_durations_seconds_count{circuit="test",success="false"} 1
	                  # HELP hoglet_circuit_dropped_calls_total Total number of calls with an open circuit (i.e.: calls that did not reach the wrapped function)
                      # TYPE hoglet_circuit_dropped_calls_total counter
                      hoglet_circuit_dropped_calls_total{cause="circuit_open",circuit="test"} 1
                      # HELP hoglet_circuit_inflight_calls_current Current number of calls in-flight
                      # TYPE hoglet_circuit_inflight_calls_current gauge
                      hoglet_circuit_inflight_calls_current{circuit="test"} 0
                     `

	if err := testutil.GatherAndCompare(reg, strings.NewReader(durationsOut1)); err != nil {
		t.Fatal(err)
	}
}
