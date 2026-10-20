package gateway_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/gateway"
	"github.com/andrewmccall/sproozi/internal/telemetry"
	"github.com/andrewmccall/sproozi/internal/telemetry/telemetrytest"
)

func TestDispatcherTracesRequestInExecutionTraceAndCorrelatesAudit(t *testing.T) {
	recorder := telemetrytest.Install(t)
	var events bytes.Buffer
	logger := telemetry.AuditWriter{Next: audit.NewLogger(&events)}
	const capability = api.CapabilityKubernetesRead
	d := gateway.Dispatcher{fixtureAuthority: {Capability: string(capability), Tier: audit.TierSemantic,
		AuditLogger: logger, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			event := gateway.AuditEvent(r.Context(), "kubernetes.get")
			event.Allowed = true
			logger.Log(event)
			w.WriteHeader(http.StatusNoContent)
		})}}
	grant := &gateway.ExecutionGrant{ExecutionID: "execution-1",
		Execution:    gateway.Reference{Backend: gateway.BackendDocker, Name: "visit-1", UID: fixtureExecutionUID},
		Policy:       gateway.PolicySnapshot{Name: fixturePolicyName, Spec: api.AgentPolicySpec{AllowedCapabilities: []api.CapabilityKind{capability}}},
		Template:     gateway.TemplateSnapshot{Name: fixtureTemplateName},
		Capabilities: []api.CapabilityKind{capability}}
	req := httptest.NewRequest(http.MethodGet, "https://service.test/api", nil)
	d.ServeHTTP(httptest.NewRecorder(), req.WithContext(gateway.WithIdentity(req.Context(), grant)))

	spans := recorder.Spans()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	span := spans[0]
	root := strings.Split(telemetry.TraceParent("execution-1"), "-")
	if span.Name() != "GET service.test:443" || span.SpanKind() != trace.SpanKindServer ||
		span.SpanContext().TraceID().String() != root[1] || span.Parent().SpanID().String() != root[2] {
		t.Fatalf("span %q kind=%v trace=%s parent=%s is not a server span under the execution root %v",
			span.Name(), span.SpanKind(), span.SpanContext().TraceID(), span.Parent().SpanID(), root)
	}
	attrs := attribute.NewSet(span.Attributes()...)
	for key, want := range map[string]attribute.Value{
		telemetry.ExecutionID: attribute.StringValue("execution-1"), telemetry.Capability: attribute.StringValue(string(capability)),
		telemetry.Operation: attribute.StringValue("kubernetes.get"), telemetry.Allowed: attribute.BoolValue(true),
		telemetry.HTTPResponseStatus: attribute.IntValue(http.StatusNoContent), telemetry.RunID: attribute.StringValue("visit-1"),
	} {
		if got, ok := attrs.Value(attribute.Key(key)); !ok || got != want {
			t.Fatalf("span attribute %s = %v, want %v", key, got.String(), want.String())
		}
	}

	var event audit.Event
	if err := json.Unmarshal(events.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if event.TraceID != root[1] || event.SpanID != span.SpanContext().SpanID().String() {
		t.Fatalf("audit trace=%s span=%s, want %s %s", event.TraceID, event.SpanID, root[1], span.SpanContext().SpanID())
	}
	logs := recorder.Logs()
	if len(logs) != 1 || logs[0].EventName() != "sproozi.gateway.decision" || logs[0].TraceID().String() != root[1] {
		t.Fatalf("decision log records = %d, want one correlated sproozi.gateway.decision", len(logs))
	}
	if _, ok := recorder.Metric(t, "sproozi.gateway.request.duration"); !ok {
		t.Fatal("request duration metric missing")
	}
	if _, ok := recorder.Metric(t, "sproozi.gateway.decisions"); !ok {
		t.Fatal("decision metric missing")
	}
}
