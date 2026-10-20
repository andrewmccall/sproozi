package controller_test

import (
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/telemetry"
	"github.com/andrewmccall/sproozi/internal/telemetry/telemetrytest"
)

func TestRunWithoutSessionTracesItsTransitionsUnderItsOwnRoot(t *testing.T) {
	recorder := telemetrytest.Install(t)
	run := newRun("traced")
	c := fake.NewClientBuilder().WithScheme(newScheme()).WithObjects(append(fixtures(), run)...).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).Build()
	if _, err := reconcileRun(t, c, "traced"); err != nil {
		t.Fatal(err)
	}
	root := strings.Split(telemetry.TraceParent(string(run.UID)), "-")
	var rootSeen, transitionSeen bool
	for _, span := range recorder.Spans() {
		if span.SpanContext().TraceID().String() != root[1] {
			continue
		}
		attrs := attribute.NewSet(span.Attributes()...)
		switch span.Name() {
		case "invoke_agent":
			rootSeen = span.SpanContext().SpanID().String() == root[2]
		case "transition AgentRun":
			phase, _ := attrs.Value("sproozi.agentrun.phase")
			transitionSeen = span.Parent().SpanID().String() == root[2] && phase.AsString() == string(sprooziv1alpha1.AgentRunPhaseQueued)
		}
	}
	if !rootSeen || !transitionSeen {
		t.Fatalf("root=%v queued transition under root=%v", rootSeen, transitionSeen)
	}
}
