// Package telemetry exports Sproozi's own traces, metrics and logs over
// OTLP/HTTP. It is configured only by the standard OTEL_* environment and is a
// no-op until an endpoint or exporter is named. Telemetry never carries prompts,
// responses, tool arguments, tool results or credentials.
package telemetry

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// ScopeName is the instrumentation scope of every Sproozi span, metric and log.
const ScopeName = "github.com/andrewmccall/sproozi"

// Tracer, Meter and Logger resolve the global providers on each call, so
// packages may hold them before Setup runs.
func Tracer() trace.Tracer           { return otel.Tracer(ScopeName) }
func Meter() metric.Meter            { return otel.Meter(ScopeName) }
func Logger() otellog.Logger         { return otel.Logger(ScopeName) }
func exporterKey(name string) string { return "OTEL_" + name + "_EXPORTER" }
func endpoint(name string) string    { return "OTEL_EXPORTER_OTLP_" + name + "_ENDPOINT" }

// Stop flushes and stops the providers Setup installed. A nil Stop means
// telemetry is off.
type Stop func(context.Context) error

// Setup installs global providers for each signal that the environment enables
// and returns their Stop. Telemetry is optional: without configuration it
// returns nil, and a configuration it cannot honour is reported on stderr and
// also returns nil, so the process always proceeds without telemetry.
func Setup(ctx context.Context, service string) Stop {
	stop, err := Configure(ctx, service)
	if err != nil {
		fmt.Fprintln(os.Stderr, "Telemetry disabled:", err)
		return nil
	}
	return stop
}

// Configure is Setup without the fallback: it returns the configuration error.
// Only OTLP over HTTP with protobuf is supported, which every OpenTelemetry
// Collector accepts.
func Configure(ctx context.Context, service string) (Stop, error) {
	signals := map[string]bool{}
	for _, name := range []string{"TRACES", "METRICS", "LOGS"} {
		enabled, err := enabledSignal(name)
		if err != nil {
			return nil, err
		}
		signals[name] = enabled
	}
	if !signals["TRACES"] && !signals["METRICS"] && !signals["LOGS"] {
		return nil, nil
	}
	res, err := resource.New(ctx, resource.WithAttributes(semconv.ServiceName(service)),
		resource.WithFromEnv(), resource.WithTelemetrySDK())
	if err != nil {
		return nil, fmt.Errorf("telemetry resource: %w", err)
	}
	var shutdowns []func(context.Context) error
	shutdown := func(ctx context.Context) error {
		errs := make([]error, 0, len(shutdowns))
		for _, stop := range shutdowns {
			errs = append(errs, stop(ctx))
		}
		return errors.Join(errs...)
	}
	if signals["TRACES"] {
		exporter, err := otlptracehttp.New(ctx)
		if err != nil {
			return nil, fmt.Errorf("telemetry traces: %w", err)
		}
		provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter),
			sdktrace.WithResource(res), sdktrace.WithIDGenerator(idGenerator{}))
		otel.SetTracerProvider(provider)
		shutdowns = append(shutdowns, provider.Shutdown)
	}
	if signals["METRICS"] {
		exporter, err := otlpmetrichttp.New(ctx)
		if err != nil {
			_ = shutdown(ctx)
			return nil, fmt.Errorf("telemetry metrics: %w", err)
		}
		provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exporter)),
			sdkmetric.WithResource(res))
		otel.SetMeterProvider(provider)
		shutdowns = append(shutdowns, provider.Shutdown)
	}
	if signals["LOGS"] {
		exporter, err := otlploghttp.New(ctx)
		if err != nil {
			_ = shutdown(ctx)
			return nil, fmt.Errorf("telemetry logs: %w", err)
		}
		provider := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter)),
			sdklog.WithResource(res))
		otel.SetLoggerProvider(provider)
		shutdowns = append(shutdowns, provider.Shutdown)
	}
	return shutdown, nil
}

// Shutdown flushes within a short bound so process exit is never held open
// by an unreachable collector.
func Shutdown(stop Stop) {
	if stop == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := stop(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "telemetry shutdown:", err)
	}
}

// enabledSignal follows the OpenTelemetry environment contract: OTEL_SDK_DISABLED
// and OTEL_<SIGNAL>_EXPORTER=none disable a signal, and a configured OTLP endpoint
// enables it. Unlike the SDK default, an unset endpoint never means localhost.
func enabledSignal(name string) (bool, error) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_SDK_DISABLED")), "true") {
		return false, nil
	}
	switch exporter := strings.TrimSpace(os.Getenv(exporterKey(name))); exporter {
	case "none":
		return false, nil
	case "", "otlp":
	default:
		return false, fmt.Errorf("%s=%q is unsupported; use otlp or none", exporterKey(name), exporter)
	}
	for _, key := range []string{"OTEL_EXPORTER_OTLP_PROTOCOL", "OTEL_EXPORTER_OTLP_" + name + "_PROTOCOL"} {
		if protocol := strings.TrimSpace(os.Getenv(key)); protocol != "" && protocol != "http/protobuf" {
			return false, fmt.Errorf("%s=%q is unsupported; use http/protobuf", key, protocol)
		}
	}
	return os.Getenv(exporterKey(name)) == "otlp" || os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") != "" ||
		os.Getenv(endpoint(name)) != "", nil
}

// ExecutionContext parents ctx on the execution's trace. The trace and root span
// IDs derive from the execution ID, so every process that serves the execution
// joins one trace without sharing state. StartExecution emits that root span.
func ExecutionContext(ctx context.Context, executionID string) context.Context {
	if executionID == "" {
		return ctx
	}
	traceID, spanID := executionIDs(executionID)
	return trace.ContextWithRemoteSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled, Remote: true,
	}))
}

// TraceParent is the W3C traceparent of the execution's root span, passed to
// harnesses so their own spans join the execution trace.
func TraceParent(executionID string) string {
	traceID, spanID := executionIDs(executionID)
	return "00-" + traceID.String() + "-" + spanID.String() + "-01"
}

// StartExecution emits the execution's root invoke_agent span with the derived
// IDs. Its owner calls it once, when it creates the execution. Durable records
// keep no start time, so the root marks creation and the trace's later spans
// give its extent.
func StartExecution(ctx context.Context, executionID string, attrs ...attribute.KeyValue) {
	if executionID == "" {
		return
	}
	traceID, spanID := executionIDs(executionID)
	ctx = context.WithValue(ctx, rootIDsKey{}, [2][]byte{traceID[:], spanID[:]})
	attrs = append(attrs, attribute.String(GenAIOperationName, "invoke_agent"), attribute.String(ExecutionID, executionID))
	_, span := Tracer().Start(ctx, "invoke_agent", trace.WithNewRoot(),
		trace.WithSpanKind(trace.SpanKindInternal), trace.WithAttributes(attrs...))
	span.End()
}

// WithinExecution parents ctx on the execution trace unless it already has a span.
func WithinExecution(ctx context.Context, executionID string) context.Context {
	if trace.SpanContextFromContext(ctx).IsValid() {
		return ctx
	}
	return ExecutionContext(ctx, executionID)
}

func executionIDs(executionID string) (trace.TraceID, trace.SpanID) {
	sum := sha256.Sum256([]byte("sproozi.execution\x00" + executionID))
	var traceID trace.TraceID
	var spanID trace.SpanID
	copy(traceID[:], sum[:16])
	copy(spanID[:], sum[16:24])
	return traceID, spanID
}

type rootIDsKey struct{}

// idGenerator is random except for an execution root span, whose IDs are fixed
// by StartExecution so that children exported earlier find their parent.
type idGenerator struct{}

func (idGenerator) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	var traceID trace.TraceID
	if ids, ok := ctx.Value(rootIDsKey{}).([2][]byte); ok {
		var spanID trace.SpanID
		copy(traceID[:], ids[0])
		copy(spanID[:], ids[1])
		return traceID, spanID
	}
	_, _ = rand.Read(traceID[:])
	return traceID, idGenerator{}.NewSpanID(ctx, traceID)
}

func (idGenerator) NewSpanID(context.Context, trace.TraceID) trace.SpanID {
	var spanID trace.SpanID
	_, _ = rand.Read(spanID[:])
	return spanID
}

// ShutdownOnSignal flushes telemetry and exits when the process is asked to
// stop, for servers whose main loop never returns.
func ShutdownOnSignal(stop Stop) {
	if stop == nil {
		return // Keep the process's default signal handling.
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, os.Interrupt)
	go func() {
		<-signals
		Shutdown(stop)
		os.Exit(0)
	}()
}

// IDGenerator returns the generator Setup installs, for providers built elsewhere.
func IDGenerator() sdktrace.IDGenerator { return idGenerator{} }
