package session

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/andrewmccall/sproozi/internal/telemetry"
	"github.com/andrewmccall/sproozi/internal/telemetry/telemetrytest"
)

func TestExecutionLifecycleSharesOneTraceUnderItsRoot(t *testing.T) {
	recorder := telemetrytest.Install(t)
	c, _, _, _, start := setup(t)
	if _, err := c.Move(t.Context(), start.Source.ExecutionID, uuid.NewString(), testRemote); err != nil {
		t.Fatal(err)
	}
	root := strings.Split(telemetry.TraceParent(start.Source.ExecutionID), "-")
	byName := map[string][]sdktrace.ReadOnlySpan{}
	for _, span := range recorder.Spans() {
		if span.SpanContext().TraceID().String() == root[1] {
			byName[span.Name()] = append(byName[span.Name()], span)
		}
	}
	for name, count := range map[string]int{"invoke_agent": 1, "start execution": 1, "finalize execution": 1,
		"move execution": 1, "prepare visit": 2, "activate visit": 2, "stop visit": 1} {
		if len(byName[name]) != count {
			t.Fatalf("%q spans in execution trace = %d, want %d (have %v)", name, len(byName[name]), count, byName)
		}
	}
	move := byName["move execution"][0]
	if move.Parent().SpanID().String() != root[2] {
		t.Fatal("operation span is not a child of the execution root")
	}
	children := 0
	// The move starts from the finalized, idle execution, so it has no stop stage.
	for _, name := range []string{"prepare visit", "activate visit"} {
		for _, span := range byName[name] {
			if span.Parent().SpanID() == move.SpanContext().SpanID() {
				children++
			}
		}
	}
	if children != 2 {
		t.Fatalf("move has %d visit stage children, want prepare and activate", children)
	}
}
