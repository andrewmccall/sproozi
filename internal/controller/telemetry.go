package controller

import (
	"context"

	"go.opentelemetry.io/otel/attribute"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/telemetry"
)

// traceTransition records a persisted phase change in its execution's trace. A
// run that is its own execution also gets its root span when it is first queued.
func (r *AgentRunReconciler) traceTransition(ctx context.Context, run *sprooziv1alpha1.AgentRun,
	from sprooziv1alpha1.AgentRunPhase, reason string,
) {
	execution := registry.ExecutionForRun(ctx, r.Client, run)
	attrs := []attribute.KeyValue{attribute.String(telemetry.ExecutionID, execution),
		attribute.String(telemetry.RunID, run.Namespace+"/"+run.Name), attribute.String(telemetry.RunUID, string(run.UID)),
		attribute.String(telemetry.Backend, "kubernetes")}
	if from == "" && execution == string(run.UID) {
		telemetry.StartExecution(ctx, execution, append(attrs,
			attribute.String("sproozi.template.name", run.Spec.TemplateRef.Name))...)
	}
	_, span := telemetry.Tracer().Start(telemetry.ExecutionContext(ctx, execution), "transition AgentRun")
	span.SetAttributes(append(attrs, attribute.String("sproozi.agentrun.from", string(from)),
		attribute.String("sproozi.agentrun.phase", string(run.Status.Phase)),
		attribute.String("sproozi.agentrun.reason", reason))...)
	span.End()
}
