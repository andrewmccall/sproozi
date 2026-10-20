package tasks

import (
	"context"
	"slices"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/andrewmccall/sproozi/internal/telemetry"
)

var (
	toolNames            = []string{submitTool, "status", "wait", "cancel"}
	operationDuration, _ = telemetry.Meter().Float64Histogram("mcp.server.operation.duration",
		metric.WithUnit("s"), metric.WithDescription("Duration of MCP requests the task service handled"),
		metric.WithExplicitBucketBoundaries(0.01, 0.02, 0.05, 0.1, 0.2, 0.5, 1, 2, 5, 10, 30, 60, 120, 300))
)

// observe follows the MCP semantic conventions: one server span per request,
// parented on the caller's params._meta trace context. The caller is an
// authenticated assistant. Only fixed tool names and methods become attributes.
func observe(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		start := time.Now()
		attrs := []attribute.KeyValue{attribute.String(telemetry.MCPMethodName, method)}
		name := method
		carrier := propagation.MapCarrier{}
		if params, ok := req.GetParams().(*mcp.CallToolParamsRaw); ok {
			for key, value := range params.Meta {
				if text, ok := value.(string); ok && (key == "traceparent" || key == "tracestate") {
					carrier[key] = text
				}
			}
			tool := "unknown"
			if slices.Contains(toolNames, params.Name) {
				tool = params.Name
			}
			name += " " + tool
			attrs = append(attrs, attribute.String(telemetry.GenAIToolName, tool),
				attribute.String(telemetry.GenAIOperationName, "execute_tool"))
		}
		ctx = propagation.TraceContext{}.Extract(ctx, carrier)
		ctx, span := telemetry.Tracer().Start(ctx, name, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(attrs...))
		result, err := next(ctx, method, req)
		if failed, ok := result.(*mcp.CallToolResult); err != nil || ok && failed != nil && failed.IsError {
			attrs = append(attrs, attribute.String(telemetry.ErrorType, "tool_error"))
			span.SetAttributes(attribute.String(telemetry.ErrorType, "tool_error"))
			span.SetStatus(codes.Error, "tool_error")
		}
		span.End()
		operationDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
		return result, err
	}
}
