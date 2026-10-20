package telemetry_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/telemetry"
	"github.com/andrewmccall/sproozi/internal/telemetry/telemetrytest"
)

func TestModelDecisionNamesChatSpanAndRecordsTokenUsage(t *testing.T) {
	recorder := telemetrytest.Install(t)
	ctx, span := telemetry.Tracer().Start(context.Background(), "POST api.openai.com:443")
	event := audit.Event{Operation: "/v1/responses", Service: "openai", Allowed: true,
		Model: "gpt-test", InputTokens: 12, OutputTokens: 30, TokensUsed: 42}.WithContext(ctx)
	var out bytes.Buffer
	telemetry.AuditWriter{Next: audit.NewLogger(&out)}.Log(event)
	span.End()

	got := recorder.Spans()[0]
	attrs := attribute.NewSet(got.Attributes()...)
	if got.Name() != "chat gpt-test" {
		t.Fatalf("span name %q, want chat gpt-test", got.Name())
	}
	for key, want := range map[string]attribute.Value{
		telemetry.GenAIOperationName: attribute.StringValue("chat"), telemetry.GenAIProviderName: attribute.StringValue("openai"),
		telemetry.GenAIRequestModel: attribute.StringValue("gpt-test"), telemetry.GenAIInputTokens: attribute.Int64Value(12),
		telemetry.GenAIOutputTokens: attribute.Int64Value(30),
	} {
		if v, ok := attrs.Value(attribute.Key(key)); !ok || v != want {
			t.Fatalf("%s = %v, want %v", key, v.String(), want.String())
		}
	}
	data, ok := recorder.Metric(t, "gen_ai.client.token.usage")
	if !ok {
		t.Fatal("token usage metric missing")
	}
	sums := map[string]int64{}
	for _, point := range data.(metricdata.Histogram[int64]).DataPoints {
		kind, _ := point.Attributes.Value(telemetry.GenAITokenType)
		model, _ := point.Attributes.Value(telemetry.GenAIRequestModel)
		if model.AsString() == "gpt-test" {
			sums[kind.AsString()] += point.Sum
		}
	}
	if sums["input"] != 12 || sums["output"] != 30 {
		t.Fatalf("token usage by type = %v, want input 12 output 30", sums)
	}
	if !strings.Contains(out.String(), `"model":"gpt-test"`) {
		t.Fatalf("audit record lost the model: %s", out.String())
	}
}

func TestWebhookDecisionBecomesLogRecord(t *testing.T) {
	recorder := telemetrytest.Install(t)
	var out bytes.Buffer
	telemetry.AuditWriter{Next: audit.NewLogger(&out)}.LogWebhook(audit.WebhookEvent{Timestamp: time.Now(),
		TemplateNamespace: "ns", TemplateName: "tmpl", Operation: "webhook.create", DenyReason: "replayed"})
	logs := recorder.Logs()
	if len(logs) != 1 || logs[0].EventName() != "sproozi.webhook.decision" || out.Len() == 0 {
		t.Fatalf("webhook logs = %d, stdout=%q", len(logs), out.String())
	}
}
