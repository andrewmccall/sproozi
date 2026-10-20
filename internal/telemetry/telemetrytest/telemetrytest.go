// Package telemetrytest installs in-memory telemetry providers for tests.
package telemetrytest

import (
	"context"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/andrewmccall/sproozi/internal/telemetry"
)

// Recorder holds every span, metric and log the process emits.
type Recorder struct {
	spans   *tracetest.SpanRecorder
	metrics *sdkmetric.ManualReader
	mu      sync.Mutex
	logs    []sdklog.Record
}

var (
	once     sync.Once
	recorder *Recorder
)

// Install sets the global providers once per test binary, because instruments
// bind to the first global provider, and clears recorded spans and logs.
func Install(t *testing.T) *Recorder {
	t.Helper()
	once.Do(func() {
		recorder = &Recorder{spans: tracetest.NewSpanRecorder(), metrics: sdkmetric.NewManualReader()}
		otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder.spans),
			sdktrace.WithIDGenerator(telemetry.IDGenerator())))
		otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(recorder.metrics)))
		otel.SetLoggerProvider(sdklog.NewLoggerProvider(sdklog.WithProcessor(recorder)))
	})
	recorder.mu.Lock()
	recorder.logs = nil
	recorder.mu.Unlock()
	recorder.spans.Reset()
	return recorder
}

// Spans returns spans that have ended since Install.
func (r *Recorder) Spans() []sdktrace.ReadOnlySpan { return r.spans.Ended() }

// Logs returns log records emitted since Install.
func (r *Recorder) Logs() []sdklog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]sdklog.Record(nil), r.logs...)
}

// Metric returns the named metric's data, cumulative since the binary started.
func (r *Recorder) Metric(t *testing.T, name string) (metricdata.Aggregation, bool) {
	t.Helper()
	var data metricdata.ResourceMetrics
	if err := r.metrics.Collect(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	for _, scope := range data.ScopeMetrics {
		for _, m := range scope.Metrics {
			if m.Name == name {
				return m.Data, true
			}
		}
	}
	return nil, false
}

func (r *Recorder) OnEmit(_ context.Context, record *sdklog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, record.Clone())
	return nil
}

func (r *Recorder) Shutdown(context.Context) error   { return nil }
func (r *Recorder) ForceFlush(context.Context) error { return nil }

func (r *Recorder) Enabled(context.Context, sdklog.EnabledParameters) bool { return true }
