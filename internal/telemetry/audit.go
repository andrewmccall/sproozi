package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/andrewmccall/sproozi/internal/audit"
)

var (
	decisions, _ = Meter().Int64Counter("sproozi.gateway.decisions",
		metric.WithUnit("{decision}"), metric.WithDescription("Gateway audit decisions"))
	tokenUsage, _ = Meter().Int64Histogram("gen_ai.client.token.usage",
		metric.WithUnit("{token}"), metric.WithDescription("Model tokens settled by the gateway"),
		metric.WithExplicitBucketBoundaries(1, 4, 16, 64, 256, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304))
	webhookDecisions, _ = Meter().Int64Counter("sproozi.webhook.decisions",
		metric.WithUnit("{decision}"), metric.WithDescription("Webhook ingress decisions"))
)

// AuditWriter makes the audit record the single source of call telemetry. Each
// decision annotates the span it was made in, becomes an OTel log record and
// updates the decision and token metrics, then reaches the next writer
// unchanged. Audit records exclude content by contract, so telemetry does too.
type AuditWriter struct {
	Next interface {
		audit.Writer
		audit.WebhookWriter
	}
}

func (w AuditWriter) Log(e audit.Event) {
	ctx := e.Context()
	attrs := eventAttributes(e)
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(attrs...)
		if provider := genAIProvider(e); provider != "" && e.Model != "" {
			span.SetName("chat " + e.Model)
			span.SetAttributes(attribute.String(GenAIOperationName, "chat"),
				attribute.String(GenAIProviderName, provider), attribute.String(GenAIRequestModel, e.Model),
				attribute.Int64(GenAIInputTokens, e.InputTokens), attribute.Int64(GenAIOutputTokens, e.OutputTokens))
		}
	}
	decisions.Add(ctx, 1, metric.WithAttributes(attribute.String(Capability, e.Capability),
		attribute.String(Tier, string(e.SemanticLevel)), attribute.String(Service, e.Service),
		attribute.Bool(Allowed, e.Allowed)))
	if provider := genAIProvider(e); provider != "" && e.Allowed && e.Model != "" {
		for kind, count := range map[string]int64{"input": e.InputTokens, "output": e.OutputTokens} {
			if count > 0 {
				tokenUsage.Record(ctx, count, metric.WithAttributes(attribute.String(GenAIOperationName, "chat"),
					attribute.String(GenAIProviderName, provider), attribute.String(GenAIRequestModel, e.Model),
					attribute.String(GenAITokenType, kind)))
			}
		}
	}
	emit(ctx, "sproozi.gateway.decision", e.Timestamp, e.Allowed, e.Operation, attrs)
	w.Next.Log(e)
}

func (w AuditWriter) LogWebhook(e audit.WebhookEvent) {
	attrs := []attribute.KeyValue{
		attribute.String("sproozi.template.namespace", e.TemplateNamespace),
		attribute.String("sproozi.template.name", e.TemplateName),
		attribute.String(Operation, e.Operation), attribute.Bool(Allowed, e.Allowed),
	}
	webhookDecisions.Add(context.Background(), 1, metric.WithAttributes(attrs...))
	if e.DenyReason != "" {
		attrs = append(attrs, attribute.String(DenyReason, e.DenyReason))
	}
	if e.RunName != "" {
		attrs = append(attrs, attribute.String(RunID, e.RunName))
	}
	emit(context.Background(), "sproozi.webhook.decision", e.Timestamp, e.Allowed, e.Operation, attrs)
	w.Next.LogWebhook(e)
}

func eventAttributes(e audit.Event) []attribute.KeyValue {
	attrs := []attribute.KeyValue{attribute.String(Operation, e.Operation), attribute.Bool(Allowed, e.Allowed)}
	for key, value := range map[string]string{
		Capability: e.Capability, Tier: string(e.SemanticLevel), Service: e.Service, Resource: e.Resource,
		DenyReason: e.DenyReason, RunID: e.RunID, RunUID: e.RunUID, Backend: e.ExecutionBackend, Policy: e.PolicyName,
	} {
		if value != "" {
			attrs = append(attrs, attribute.String(key, value))
		}
	}
	if e.UpstreamStatus != 0 {
		attrs = append(attrs, attribute.Int("sproozi.upstream.status_code", e.UpstreamStatus))
	}
	if e.TokensUsed != 0 {
		attrs = append(attrs, attribute.Int64("sproozi.tokens.used", e.TokensUsed))
	}
	return attrs
}

// genAIProvider names the provider for model records, which the model handler
// labels openai or anthropic, matching gen_ai.provider.name values.
func genAIProvider(e audit.Event) string {
	if e.Service == "openai" || e.Service == "anthropic" {
		return e.Service
	}
	return ""
}

func emit(ctx context.Context, event string, at time.Time, allowed bool, body string, attrs []attribute.KeyValue) {
	var record otellog.Record
	record.SetEventName(event)
	record.SetTimestamp(at)
	record.SetObservedTimestamp(time.Now())
	record.SetSeverity(otellog.SeverityInfo)
	if !allowed {
		record.SetSeverity(otellog.SeverityWarn)
	}
	record.SetBody(attribute.StringValue(body))
	record.AddAttributes(attrs...)
	Logger().Emit(ctx, record)
}
