package controller

import (
	"context"
	"errors"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	ctrl "sigs.k8s.io/controller-runtime"
)

// resolveSession applies trusted template registration before admission or
// provisioning. A nonnil result means the caller must return without launching.
func (r *AgentRunReconciler) resolveSession(ctx context.Context, run *api.AgentRun, template *api.AgentTemplate) (
	*registry.Record, *ctrl.Result, error,
) {
	record, err := registry.ResolveRun(ctx, r.Client, run, template)
	if err == nil {
		return record, nil, nil
	}
	if errors.Is(err, registry.ErrBindingPending) {
		return nil, &ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if errors.Is(err, registry.ErrSessionStopped) && record != nil {
		// A completed session stop is a normal end, not a binding conflict:
		// handleRunning waits for the sandbox to finish, then ends the run.
		return record, nil, nil
	}
	phase, reason := api.AgentRunPhaseFailed, "SessionBindingRejected"
	message := "AgentRun does not match the session registration for managed AgentTemplate " + template.Name
	switch {
	case errors.Is(err, registry.ErrExpired):
		phase, reason = api.AgentRunPhaseTimedOut, "SessionDeadlineExceeded"
		message = "Execution for this AgentRun passed its deadline"
	case errors.Is(err, registry.ErrSessionCancelled):
		// An abort cancels the registration before it sets spec.cancel, so a
		// reconcile in between sees only the registration. The run was
		// cancelled, not rejected.
		phase, reason = api.AgentRunPhaseCancelled, "SessionCancelled"
		message = "Session for this AgentRun was cancelled"
	case errors.Is(err, registry.ErrGone):
		message = "Session registration for this AgentRun no longer exists"
	case !errors.Is(err, registry.ErrConflict):
		return nil, nil, err
	}
	result, transitionErr := r.transitionPhase(ctx, run, phase, reason, message)
	return nil, &result, transitionErr
}
