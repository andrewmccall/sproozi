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
	phase, reason := api.AgentRunPhaseFailed, "SessionBindingRejected"
	if errors.Is(err, registry.ErrExpired) {
		phase, reason = api.AgentRunPhaseTimedOut, "SessionDeadlineExceeded"
	} else if !errors.Is(err, registry.ErrConflict) && !errors.Is(err, registry.ErrGone) {
		return nil, nil, err
	}
	result, transitionErr := r.transitionPhase(ctx, run, phase, reason, "")
	return nil, &result, transitionErr
}
