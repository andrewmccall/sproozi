package gateway

import (
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/andrewmccall/sproozi/internal/telemetry"
)

var requestDuration, _ = telemetry.Meter().Float64Histogram("sproozi.gateway.request.duration",
	metric.WithUnit("s"), metric.WithDescription("Duration of inspected gateway requests"),
	metric.WithExplicitBucketBoundaries(0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300))

// startRequest opens the server span of one inspected request inside the
// execution's trace. Workload trace headers are never a parent: the sandbox is
// untrusted, and proxytransport strips them before any upstream sees them.
func startRequest(w http.ResponseWriter, r *http.Request, identity *ExecutionGrant, route Route) (
	http.ResponseWriter, *http.Request, func(),
) {
	start := time.Now()
	attrs := append(identity.TelemetryAttributes(),
		attribute.String(telemetry.HTTPRequestMethod, r.Method),
		attribute.String(telemetry.ServerAddress, normalizeAuthority(r.Host)),
		attribute.String(telemetry.Tier, string(route.Tier)))
	if route.Capability != "" {
		attrs = append(attrs, attribute.String(telemetry.Capability, route.Capability))
	}
	ctx := telemetry.ExecutionContext(r.Context(), identity.ExecutionID)
	ctx, span := telemetry.Tracer().Start(ctx, r.Method+" "+normalizeAuthority(r.Host),
		trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
	recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	return recorder, r.WithContext(ctx), func() {
		span.SetAttributes(attribute.Int(telemetry.HTTPResponseStatus, recorder.status))
		if recorder.status >= http.StatusInternalServerError {
			span.SetStatus(codes.Error, strconv.Itoa(recorder.status))
		}
		span.End()
		requestDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(
			attribute.String(telemetry.Capability, route.Capability), attribute.String(telemetry.Tier, string(route.Tier)),
			attribute.Int(telemetry.HTTPResponseStatus, recorder.status)))
	}
}

// TelemetryAttributes identify the grant's execution and visit in telemetry.
func (i *ExecutionGrant) TelemetryAttributes() []attribute.KeyValue {
	event := i.auditEvent("")
	return []attribute.KeyValue{
		attribute.String(telemetry.ExecutionID, i.ExecutionID),
		attribute.String(telemetry.RunID, event.RunID),
		attribute.String(telemetry.RunUID, event.RunUID),
		attribute.String(telemetry.Backend, event.ExecutionBackend),
		attribute.String(telemetry.Policy, event.PolicyName),
	}
}

// statusRecorder keeps streaming intact: handlers still reach Flush, and
// http.ResponseController reaches the original writer through Unwrap.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (s *statusRecorder) WriteHeader(status int) {
	if !s.wroteHeader {
		s.status, s.wroteHeader = status, true
	}
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(p []byte) (int, error) {
	s.wroteHeader = true
	return s.ResponseWriter.Write(p)
}

func (s *statusRecorder) Flush() {
	if flusher, ok := s.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
