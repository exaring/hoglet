package hogprom

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/exaring/hoglet"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
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

func TestWithPrometheusMetrics(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewCollector("test")
		of, err := m.Wrap(&mockObserverFactory{})
		require.NoError(t, err)

		inflightOut0 := `# HELP hoglet_circuit_inflight_calls_current Current number of calls in-flight
                     # TYPE hoglet_circuit_inflight_calls_current gauge
                     hoglet_circuit_inflight_calls_current{circuit="test"} 0
                    `

		if err := testutil.CollectAndCompare(m, strings.NewReader(inflightOut0)); err != nil {
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
		if err := testutil.CollectAndCompare(m, strings.NewReader(droppedOut1)); err != nil {
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
		if err := testutil.CollectAndCompare(m, strings.NewReader(inflightOut1)); err != nil {
			t.Fatal(err)
		}

		time.Sleep(time.Second) // advance the fake clock 1 second forward

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

		if err := testutil.CollectAndCompare(m, strings.NewReader(durationsOut1)); err != nil {
			t.Fatal(err)
		}
	})
}

// TestNativeHistogram verifies the native-histogram side of the call-duration
// metric. Native histograms are only carried in the protobuf exposition format,
// so they are invisible to the text-based testutil.CollectAndCompare used above;
// we have to inspect the dto.Metric directly to assert the native buckets exist.
func TestNativeHistogram(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewCollector("test")
		of, err := m.Wrap(&mockObserverFactory{})
		require.NoError(t, err)

		o, err := of.ObserverForCall(context.Background(), hoglet.StateClosed)
		require.NoError(t, err)

		time.Sleep(time.Second) // observe a 1s call duration
		o.Observe(true)

		ch := make(chan prometheus.Metric, 4)
		m.callDurations.Collect(ch)
		close(ch)

		var found bool
		for metric := range ch {
			var d dto.Metric
			require.NoError(t, metric.Write(&d))
			h := d.GetHistogram()

			require.Equal(t, uint64(1), h.GetSampleCount())
			require.InDelta(t, 1.0, h.GetSampleSum(), 1e-9)

			// Native-histogram assertions: a nonzero schema means dynamic buckets
			// are active (bucket factor 1.1 → schema 3), and the single 1s sample
			// must land in exactly one positive span/delta.
			require.Equal(t, int32(3), h.GetSchema(), "native bucket factor should resolve to schema 3")
			require.NotEmpty(t, h.GetPositiveSpan(), "1s observation must populate a positive native bucket")
			require.Equal(t, []int64{1}, h.GetPositiveDelta())

			found = true
		}
		require.True(t, found, "no call_durations metric was collected")
	})
}
