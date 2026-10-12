package controller

import (
	"context"
	"errors"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	placement "github.com/andrewmccall/sproozi/internal/runtime/kubernetes/session"
	ctrl "sigs.k8s.io/controller-runtime"
)

// resolveSession applies trusted template registration before admission or
// provisioning. A nonnil result means the caller must return without launching.
func (r *AgentRunReconciler) resolveSession(ctx context.Context, run *api.AgentRun, template *api.AgentTemplate) (
	*placement.Record, *ctrl.Result, error,
) {
	record, err := placement.ResolveRun(ctx, r.Client, run, template)
	if err == nil {
		return record, nil, nil
	}
	if errors.Is(err, placement.ErrBindingPending) {
		return nil, &ctrl.Result{RequeueAfter: time.Second}, nil
	}
	phase, reason := api.AgentRunPhaseFailed, "SessionBindingRejected"
	if errors.Is(err, placement.ErrExpired) {
		phase, reason = api.AgentRunPhaseTimedOut, "SessionDeadlineExceeded"
	} else if !errors.Is(err, placement.ErrConflict) && !errors.Is(err, placement.ErrGone) {
		return nil, nil, err
	}
	result, transitionErr := r.transitionPhase(ctx, run, phase, reason, "")
	return nil, &result, transitionErr
}
