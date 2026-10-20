package tasks_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/andrewmccall/sproozi/internal/tasks"
	"github.com/andrewmccall/sproozi/internal/telemetry/telemetrytest"
)

const taskToken = "0123456789abcdef0123456789abcdef"

var taskReference = map[string]any{"id": "task", "uid": "uid"}

type stubService struct{}

func (stubService) Submit(context.Context, tasks.Submit) (tasks.View, error) {
	return tasks.View{}, nil
}
func (stubService) Status(_ context.Context, r tasks.Reference) (tasks.View, error) {
	return tasks.View{Reference: r, Phase: "Running"}, nil
}
func (stubService) Wait(context.Context, tasks.Reference, time.Duration) (tasks.View, error) {
	return tasks.View{}, nil
}
func (stubService) Cancel(context.Context, tasks.Reference) (tasks.View, error) {
	return tasks.View{}, errors.New("not found")
}

type bearer struct{}

func (bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+taskToken)
	return http.DefaultTransport.RoundTrip(r)
}

func TestTaskToolsAreServerSpansUnderCallerTraceWithDuration(t *testing.T) {
	recorder := telemetrytest.Install(t)
	handler, err := tasks.NewHandler(stubService{}, taskToken)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	defer server.Close()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil).Connect(t.Context(),
		&mcp.StreamableClientTransport{Endpoint: server.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = session.Close() }()
	const parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	if _, err := session.CallTool(t.Context(), &mcp.CallToolParams{Meta: mcp.Meta{"traceparent": parent},
		Name: "status", Arguments: taskReference}); err != nil {
		t.Fatal(err)
	}
	if result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "cancel",
		Arguments: taskReference}); err != nil || !result.IsError {
		t.Fatalf("cancel result %v %v", result, err)
	}
	var status, cancel trace.SpanContext
	var cancelFailed bool
	for _, span := range recorder.Spans() {
		switch span.Name() {
		case "tools/call status":
			status = span.Parent()
		case "tools/call cancel":
			cancel = span.SpanContext()
			attrs := attribute.NewSet(span.Attributes()...)
			cancelFailed = attrs.HasValue("error.type")
		}
	}
	if !strings.Contains(parent, status.TraceID().String()) || !strings.Contains(parent, status.SpanID().String()) {
		t.Fatalf("status span parent %s/%s, want caller %s", status.TraceID(), status.SpanID(), parent)
	}
	if !cancel.IsValid() || !cancelFailed {
		t.Fatal("failed cancel tool call was not recorded as an error")
	}
	if _, ok := recorder.Metric(t, "mcp.server.operation.duration"); !ok {
		t.Fatal("MCP server duration metric missing")
	}
}
