package telemetry_test

import (
	"context"
	"testing"

	"github.com/andrewmccall/sproozi/internal/telemetry"
	"github.com/andrewmccall/sproozi/internal/telemetry/telemetrytest"
)

func TestTelemetryIsOffUntilConfiguredAndUnsupportedConfigurationProceedsWithout(t *testing.T) {
	for _, key := range []string{"OTEL_SDK_DISABLED", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_TRACES_EXPORTER",
		"OTEL_METRICS_EXPORTER", "OTEL_LOGS_EXPORTER", "OTEL_EXPORTER_OTLP_PROTOCOL"} {
		t.Setenv(key, "")
	}
	if stop := telemetry.Setup(context.Background(), "test"); stop != nil {
		t.Fatal("unconfigured telemetry is not off")
	}
	telemetry.Shutdown(nil)
	telemetry.ShutdownOnSignal(nil)
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector.invalid:4318")
	for key, value := range map[string]string{"OTEL_EXPORTER_OTLP_PROTOCOL": "grpc", "OTEL_METRICS_EXPORTER": "prometheus"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, value)
			if _, err := telemetry.Configure(context.Background(), "test"); err == nil {
				t.Fatalf("%s=%s accepted", key, value)
			}
			if stop := telemetry.Setup(context.Background(), "test"); stop != nil {
				t.Fatalf("%s=%s did not fall back to no telemetry", key, value)
			}
		})
	}
	t.Setenv("OTEL_SDK_DISABLED", "true")
	if stop, err := telemetry.Configure(context.Background(), "test"); stop != nil || err != nil {
		t.Fatal("OTEL_SDK_DISABLED did not turn telemetry off")
	}
}

func TestExecutionRootSpanIsTheParentOfExecutionSpans(t *testing.T) {
	recorder := telemetrytest.Install(t)
	_, child := telemetry.Tracer().Start(telemetry.ExecutionContext(context.Background(), "execution-2"), "child")
	child.End()
	telemetry.StartExecution(context.Background(), "execution-2")
	spans := recorder.Spans()
	if len(spans) != 2 {
		t.Fatalf("spans = %d", len(spans))
	}
	childSpan, rootSpan := spans[0], spans[1]
	if rootSpan.Name() != "invoke_agent" || rootSpan.Parent().IsValid() ||
		rootSpan.SpanContext().TraceID() != childSpan.SpanContext().TraceID() ||
		rootSpan.SpanContext().SpanID() != childSpan.Parent().SpanID() {
		t.Fatalf("root %s/%s does not parent child %s/%s", rootSpan.SpanContext().TraceID(),
			rootSpan.SpanContext().SpanID(), childSpan.SpanContext().TraceID(), childSpan.Parent().SpanID())
	}
	if want := "00-" + rootSpan.SpanContext().TraceID().String() + "-" + rootSpan.SpanContext().SpanID().String() + "-01"; telemetry.TraceParent("execution-2") != want {
		t.Fatalf("traceparent %s, want %s", telemetry.TraceParent("execution-2"), want)
	}
}
