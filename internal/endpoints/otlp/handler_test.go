package otlp_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	logspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	tracepb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logpb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	spanpb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/andrewmccall/sproozi/internal/endpoints/otlp"
	"github.com/andrewmccall/sproozi/internal/gateway"
)

const secret = "do-not-forward"

func text(key, value string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: value}}}
}

func number(key string, value int64) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: key, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_IntValue{IntValue: value}}}
}

type collector struct {
	status int
	paths  []string
	bodies [][]byte
	header http.Header
}

func (c *collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	c.paths, c.bodies, c.header = append(c.paths, r.URL.Path), append(c.bodies, body), r.Header.Clone()
	w.WriteHeader(c.status)
}

func relay(t *testing.T, status int) (*collector, http.Handler) {
	t.Helper()
	sink := &collector{status: status}
	server := httptest.NewServer(sink)
	t.Cleanup(server.Close)
	endpoint, _ := url.Parse(server.URL + "/base")
	return sink, otlp.Mount(otlp.NewHandler(&otlp.Config{Endpoint: endpoint,
		Headers: http.Header{"Authorization": {"Bearer collector"}}, Client: server.Client()}), nil)
}

func post(t *testing.T, handler http.Handler, path string, message proto.Message, identity bool, gzipped bool) *httptest.ResponseRecorder {
	t.Helper()
	body, err := proto.Marshal(message)
	if err != nil {
		t.Fatal(err)
	}
	if gzipped {
		var buffer bytes.Buffer
		writer := gzip.NewWriter(&buffer)
		_, _ = writer.Write(body)
		_ = writer.Close()
		body = buffer.Bytes()
	}
	req := httptest.NewRequest(http.MethodPost, "https://gateway.test"+path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-protobuf")
	if gzipped {
		req.Header.Set("Content-Encoding", "gzip")
	}
	if identity {
		req = req.WithContext(gateway.WithIdentity(req.Context(), &gateway.ExecutionGrant{ExecutionID: "execution-1",
			Execution: gateway.Reference{Backend: gateway.BackendDocker, Name: "visit-1", UID: "container-1"},
			Policy:    gateway.PolicySnapshot{Name: "home-policy"}}))
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

func attributes(list []*commonpb.KeyValue) map[string]*commonpb.AnyValue {
	out := map[string]*commonpb.AnyValue{}
	for _, kv := range list {
		out[kv.Key] = kv.Value
	}
	return out
}

func TestRelayStampsTrustedIdentityAndRemovesContent(t *testing.T) {
	sink, handler := relay(t, http.StatusOK)
	export := &tracepb.ExportTraceServiceRequest{ResourceSpans: []*spanpb.ResourceSpans{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{text("service.name", "codex"),
			text("sproozi.execution.id", "forged"), text("sproozi.telemetry.source", "gateway")}},
		ScopeSpans: []*spanpb.ScopeSpans{{Spans: []*spanpb.Span{{Name: "claude_code.tool", TraceId: bytes.Repeat([]byte{1}, 16),
			SpanId: bytes.Repeat([]byte{2}, 8), Attributes: []*commonpb.KeyValue{
				text("gen_ai.input.messages", secret), text("prompt", secret), text("tool_parameters", secret),
				text("gen_ai.tool.call.arguments", secret), text("user.email", secret), text("api_key", secret),
				text("http.request.header.authorization", secret), text("tool_name", "Bash"),
				number("gen_ai.usage.input_tokens", 5), text("long", strings.Repeat("x", 5000))},
			Events: []*spanpb.Span_Event{{Name: "tool_result", Attributes: []*commonpb.KeyValue{text("output", secret)}}}}}}},
	}, {ScopeSpans: []*spanpb.ScopeSpans{{Spans: []*spanpb.Span{{Name: "no resource"}}}}}}}
	if response := post(t, handler, "/otlp/v1/traces", export, true, true); response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body)
	}
	if len(sink.paths) != 1 || sink.paths[0] != "/base/v1/traces" || sink.header.Get("Authorization") != "Bearer collector" ||
		sink.header.Get("Content-Type") != "application/x-protobuf" {
		t.Fatalf("forwarded to %v with %v", sink.paths, sink.header)
	}
	if bytes.Contains(sink.bodies[0], []byte(secret)) {
		t.Fatal("content or credential attribute reached the collector")
	}
	var got tracepb.ExportTraceServiceRequest
	if err := proto.Unmarshal(sink.bodies[0], &got); err != nil {
		t.Fatal(err)
	}
	for _, resourceSpans := range got.ResourceSpans {
		resource := attributes(resourceSpans.Resource.GetAttributes())
		if resource["sproozi.execution.id"].GetStringValue() != "execution-1" || resource["sproozi.run.id"].GetStringValue() != "visit-1" ||
			resource["sproozi.telemetry.source"].GetStringValue() != "harness" {
			t.Fatalf("resource identity not trusted: %v", resource)
		}
	}
	if attributes(got.ResourceSpans[0].Resource.Attributes)["service.name"].GetStringValue() != "codex" {
		t.Fatal("harness service name lost")
	}
	span := attributes(got.ResourceSpans[0].ScopeSpans[0].Spans[0].Attributes)
	if span["tool_name"].GetStringValue() != "Bash" || span["gen_ai.usage.input_tokens"].GetIntValue() != 5 ||
		len(span["long"].GetStringValue()) != 1024 {
		t.Fatalf("operational attributes not kept within bounds: %v", span)
	}
}

func TestRelayRejectsUnauthenticatedOrMalformedExportsAndPassesBackpressure(t *testing.T) {
	logs := &logspb.ExportLogsServiceRequest{ResourceLogs: []*logpb.ResourceLogs{{ScopeLogs: []*logpb.ScopeLogs{{
		LogRecords: []*logpb.LogRecord{{Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "codex.api_request"}}}}}}}}}
	sink, handler := relay(t, http.StatusOK)
	if response := post(t, handler, "/otlp/v1/logs", logs, false, false); response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated export status %d", response.Code)
	}
	if response := post(t, handler, "/otlp/v1/profiles", logs, true, false); response.Code != http.StatusNotFound {
		t.Fatalf("unknown signal status %d", response.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "https://gateway.test/otlp/v1/logs", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req.WithContext(gateway.WithIdentity(req.Context(), &gateway.ExecutionGrant{ExecutionID: "e"})))
	if response.Code != http.StatusUnsupportedMediaType || len(sink.paths) != 0 {
		t.Fatalf("JSON export status %d forwarded=%d", response.Code, len(sink.paths))
	}
	busy, busyHandler := relay(t, http.StatusServiceUnavailable)
	if response := post(t, busyHandler, "/otlp/v1/logs", logs, true, false); response.Code != http.StatusServiceUnavailable || len(busy.paths) != 1 {
		t.Fatalf("collector backpressure status %d", response.Code)
	}
}
