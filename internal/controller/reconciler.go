/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	k8sresources "github.com/andrewmccall/sproozi/internal/kubernetes"
	"github.com/andrewmccall/sproozi/internal/policy"
)

const (
	defaultRetentionTTL       = 30 * 24 * time.Hour
	admittedRequeueTime       = 10 * time.Second
	agentRunFinalizer         = "sproozi.com/agentrun-cleanup"
	agentRunPhaseIndex        = "sproozi.com/agentrun-phase"
	agentRunTemplateIndex     = "sproozi.com/agentrun-template"
	agentTemplatePolicyIndex  = "sproozi.com/agenttemplate-policy"
	agentTemplateRuntimeIndex = "sproozi.com/agenttemplate-runtime"
)

// +kubebuilder:rbac:groups=sproozi.com,resources=agentruns,verbs=get;list;watch;update;patch;delete
// +kubebuilder:rbac:groups=sproozi.com,resources=agentruns/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sproozi.com,resources=agentruns/finalizers,verbs=update
// +kubebuilder:rbac:groups=sproozi.com,resources=agenttemplates,verbs=get;list;watch
// +kubebuilder:rbac:groups=sproozi.com,resources=agentpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=sproozi.com,resources=agentruntimes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get

// AgentRunReconciler reconciles AgentRun objects.
type AgentRunReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	SandboxClient k8sresources.SandboxClient
}

// RunResourcesReleaser is the lifecycle seam for revoking all authority and
// disposable state belonging to a run. Implementations are idempotent: a
// retry after a partial failure resumes at the first resource still present.
type RunResourcesReleaser interface {
	ReconcileRelease(context.Context, *sprooziv1alpha1.AgentRun) (complete bool, err error)
}

// Reconcile drives an AgentRun through its lifecycle phases.
func (r *AgentRunReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var run sprooziv1alpha1.AgentRun
	if err := r.Get(ctx, req.NamespacedName, &run); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !run.DeletionTimestamp.IsZero() {
		return r.finalizeDeletion(ctx, &run)
	}
	if !IsTerminal(run.Status.Phase) && controllerutil.AddFinalizer(&run, agentRunFinalizer) {
		if err := r.Update(ctx, &run); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Initialize runs that have no phase yet; transitionPhase sets the Ready condition.
	if run.Status.Phase == "" {
		return r.transitionPhase(ctx, &run, sprooziv1alpha1.AgentRunPhaseQueued, "Initialized", "")
	}

	// Terminal runs are only subject to retention TTL deletion.
	if IsTerminal(run.Status.Phase) {
		return r.handleTerminal(ctx, &run)
	}

	// Cancellation is honoured at any non-terminal phase.
	if run.Spec.Cancel {
		return r.transitionPhase(ctx, &run, sprooziv1alpha1.AgentRunPhaseCancelled, "CancelRequested", "")
	}

	switch run.Status.Phase {
	case sprooziv1alpha1.AgentRunPhaseQueued:
		return r.handleQueued(ctx, &run)
	case sprooziv1alpha1.AgentRunPhaseAdmitted:
		return r.handleAdmitted(ctx, &run)
	case sprooziv1alpha1.AgentRunPhaseRunning:
		return r.handleRunning(ctx, &run)
	}

	return ctrl.Result{}, nil
}

func (r *AgentRunReconciler) finalizeDeletion(ctx context.Context, run *sprooziv1alpha1.AgentRun) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(run, agentRunFinalizer) {
		return ctrl.Result{}, nil
	}
	complete, err := r.ReconcileRelease(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !complete {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	controllerutil.RemoveFinalizer(run, agentRunFinalizer)
	return ctrl.Result{}, r.Update(ctx, run)
}

// handleQueued evaluates policy, enforces the single-active-run invariant, and
// either admits the run or leaves it queued. The active-run list is cluster-wide
// to enforce the single global active-run slot (PRD §6).
func (r *AgentRunReconciler) handleQueued(ctx context.Context, run *sprooziv1alpha1.AgentRun) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	tmpl, pol, rt, err := r.resolveRefs(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}
	if tmpl == nil {
		// resolveRefs already transitioned run to Failed.
		return ctrl.Result{}, nil
	}

	if denial := policy.Evaluate(pol.Spec, tmpl.Spec, run.Spec, rt.Spec); denial != nil {
		logger.Info("Policy denied AgentRun", "name", run.Name, "reason", denial.Reason)
		_, err := r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseFailed, string(denial.Reason), denial.Message)
		return ctrl.Result{}, err
	}

	// Cluster-wide list enforces the single global active-run slot.
	var allRuns sprooziv1alpha1.AgentRunList
	if err := r.List(ctx, &allRuns); err != nil {
		return ctrl.Result{}, err
	}

	if active := ActiveRun(allRuns.Items); active != nil {
		logger.Info("Active run exists, staying queued", "name", run.Name, "activeRun", active.Name)
		return ctrl.Result{RequeueAfter: admittedRequeueTime}, nil
	}

	next := NextQueued(allRuns.Items)
	if next == nil || next.Name != run.Name || next.Namespace != run.Namespace {
		// Another queued run has an earlier timestamp; wait.
		return ctrl.Result{RequeueAfter: admittedRequeueTime}, nil
	}

	logger.Info("Admitting AgentRun", "name", run.Name)
	_, err = r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseAdmitted, "", "")
	return ctrl.Result{}, err
}

// handleAdmitted checks the execution deadline for an admitted run, provisions
// the per-run identity resources (ServiceAccount and NetworkPolicy), and
// transitions the run to Running once identity is in place. Kubernetes API
// authorization is deliberately owned by the shared gateway; the workload
// ServiceAccount is an immutable run identity only and is never bound to
// namespace Roles.
func (r *AgentRunReconciler) handleAdmitted(ctx context.Context, run *sprooziv1alpha1.AgentRun) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// StartedAt must be set when the run is admitted. If it is somehow absent
	// (e.g. a status patch was lost), set it now — do not loop forever, which
	// would deadlock the entire cluster-wide queue.
	if run.Status.StartedAt == nil {
		now := metav1.Now()
		run.Status.StartedAt = &now
		if err := r.Status().Update(ctx, run); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("Recovered missing StartedAt on Admitted run", "name", run.Name)
	}

	tmpl, pol, rt, err := r.resolveRefs(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pol == nil {
		return ctrl.Result{}, nil
	}
	if denial := policy.Evaluate(pol.Spec, tmpl.Spec, run.Spec, rt.Spec); denial != nil {
		logger.Info("Policy revoked AgentRun admission", "name", run.Name, "reason", denial.Reason)
		_, transitionErr := r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseFailed, string(denial.Reason), denial.Message)
		return ctrl.Result{}, transitionErr
	}

	deadline := run.Status.StartedAt.Add(pol.Spec.MaxExecutionDuration.Duration)
	if time.Now().After(deadline) {
		logger.Info("Admitted AgentRun deadline exceeded", "name", run.Name)
		_, err := r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseTimedOut, "MaxExecutionDurationExceeded", "")
		return ctrl.Result{}, err
	}

	if run.Status.Identity.ServiceAccountName == "" {
		saName, err := k8sresources.EnsureServiceAccount(ctx, r.Client, run)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := k8sresources.EnsureNetworkPolicy(ctx, r.Client, run, pol); err != nil {
			return ctrl.Result{}, err
		}

		run.Status.Identity = sprooziv1alpha1.AgentRunIdentity{
			ServiceAccountName: saName,
		}
		if err := r.Status().Update(ctx, run); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("Provisioned run identity", "name", run.Name, "serviceAccount", saName)
	}

	_, err = r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseRunning, "", "")
	return ctrl.Result{}, err
}

// handleRunning creates the Sandbox Pod, monitors its lifecycle, and transitions
// the run to Succeeded, Failed, or TimedOut based on sandbox phase.
func (r *AgentRunReconciler) handleRunning(ctx context.Context, run *sprooziv1alpha1.AgentRun) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	tmpl, pol, rt, err := r.resolveRefs(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pol == nil {
		return ctrl.Result{}, nil
	}
	if denial := policy.Evaluate(pol.Spec, tmpl.Spec, run.Spec, rt.Spec); denial != nil {
		logger.Info("Policy revoked running AgentRun", "name", run.Name, "reason", denial.Reason)
		_, transitionErr := r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseFailed, string(denial.Reason), denial.Message)
		return ctrl.Result{}, transitionErr
	}
	if run.Status.Identity.ServiceAccountName != "" {
		if err := k8sresources.EnsureNetworkPolicy(ctx, r.Client, run, pol); err != nil {
			return ctrl.Result{}, err
		}
	}

	if run.Status.StartedAt == nil {
		now := metav1.Now()
		run.Status.StartedAt = &now
		if err := r.Status().Update(ctx, run); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("Recovered missing StartedAt on Running run", "name", run.Name)
	}

	deadline := run.Status.StartedAt.Add(pol.Spec.MaxExecutionDuration.Duration)
	if time.Now().After(deadline) {
		logger.Info("AgentRun deadline exceeded", "name", run.Name)
		_, err := r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseTimedOut, "MaxExecutionDurationExceeded", "")
		return ctrl.Result{}, err
	}
	if err := k8sresources.EnsureAgentContract(ctx, r.Client, run, tmpl, rt); err != nil {
		return ctrl.Result{}, err
	}

	// Ensure the Sandbox Pod exists.
	sandboxName, err := r.ensureSandbox(ctx, run, rt)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Check Sandbox status.
	sandboxStatus, err := r.SandboxClient.GetStatus(ctx, sandboxName)
	if err != nil {
		return ctrl.Result{}, err
	}

	switch sandboxStatus {
	case k8sresources.SandboxStatusPending, k8sresources.SandboxStatusRunning:
		remaining := time.Until(deadline)
		return ctrl.Result{RequeueAfter: remaining}, nil

	case k8sresources.SandboxStatusSucceeded, k8sresources.SandboxStatusFailed:
		return r.processSandboxExit(ctx, run, sandboxName)

	case k8sresources.SandboxStatusUnknown:
		phase, reason := sandboxPhaseToRunPhase(sandboxStatus)
		logger.Info("Sandbox terminated abnormally", "name", run.Name, "sandboxStatus", sandboxStatus)
		_, err := r.transitionPhase(ctx, run, phase, reason, "")
		return ctrl.Result{}, err
	}

	remaining := time.Until(deadline)
	return ctrl.Result{RequeueAfter: remaining}, nil
}

// handleTerminal revokes any provisioned identity resources and then deletes
// the run once its retention TTL has elapsed. It does NOT call resolveRefs to
// avoid mutating terminal runs if their referenced resources have been deleted.
func (r *AgentRunReconciler) handleTerminal(ctx context.Context, run *sprooziv1alpha1.AgentRun) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if run.Status.Identity.ServiceAccountName != "" || run.Status.Identity.SandboxName != "" {
		complete, err := r.ReconcileRelease(ctx, run)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !complete {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		run.Status.Identity = sprooziv1alpha1.AgentRunIdentity{}
		if err := r.Status().Update(ctx, run); err != nil {
			return ctrl.Result{}, err
		}
		logger.Info("Revoked run identity", "name", run.Name)
	}

	ttl := r.retentionTTL(ctx, run)

	// Use CompletedAt as the retention baseline; fall back to CreationTimestamp.
	baseline := run.CreationTimestamp.Time
	if run.Status.CompletedAt != nil {
		baseline = run.Status.CompletedAt.Time
	}

	deleteAt := baseline.Add(ttl)
	if time.Now().After(deleteAt) {
		logger.Info("Deleting AgentRun after retention TTL", "name", run.Name, "phase", run.Status.Phase)
		if err := r.Delete(ctx, run); err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	remaining := time.Until(deleteAt)
	return ctrl.Result{RequeueAfter: remaining}, nil
}

// ReconcileRelease performs the one ordered release flow shared by terminal
// retention and explicit deletion. Network isolation is deliberately retained
// until the sandbox is observed stopped or absent.
func (r *AgentRunReconciler) ReconcileRelease(ctx context.Context, run *sprooziv1alpha1.AgentRun) (bool, error) {
	if run.Status.Identity.SandboxName != "" {
		if r.SandboxClient == nil {
			return false, errors.New("cannot release AgentRun without sandbox client")
		}
		if releaser, ok := r.SandboxClient.(interface {
			DeleteForRun(context.Context, string, string) error
		}); ok {
			if err := releaser.DeleteForRun(ctx, run.Status.Identity.SandboxName, string(run.UID)); err != nil {
				return false, err
			}
		} else if err := r.revokeSandbox(ctx, run); err != nil {
			return false, err
		}
		status, err := r.SandboxClient.GetStatus(ctx, run.Status.Identity.SandboxName)
		if err != nil {
			return false, err
		}
		if status == k8sresources.SandboxStatusPending || status == k8sresources.SandboxStatusRunning {
			return false, nil
		}
	}
	// Terminal phase is the gateway revocation point: gateway lookups reject it
	// before any disposable identity is removed.
	if err := k8sresources.RevokeAgentContract(ctx, r.Client, run, string(run.UID)); err != nil {
		return false, err
	}
	if saName := run.Status.Identity.ServiceAccountName; saName != "" {
		if err := k8sresources.RevokeNetworkPolicy(ctx, r.Client, saName, string(run.UID)); err != nil {
			return false, err
		}
		if err := k8sresources.RevokeServiceAccount(ctx, r.Client, saName, string(run.UID)); err != nil {
			return false, err
		}
	}
	return true, nil
}

// retentionTTL returns the effective retention duration for a terminal run.
// It performs a read-only lookup of the policy and never writes to the run.
// A zero or negative TTL (should not occur with CRD validation) is replaced
// with defaultRetentionTTL to prevent immediate deletion.
func (r *AgentRunReconciler) retentionTTL(ctx context.Context, run *sprooziv1alpha1.AgentRun) time.Duration {
	var tmpl sprooziv1alpha1.AgentTemplate
	if err := r.Get(ctx, types.NamespacedName{Name: run.Spec.TemplateRef.Name, Namespace: run.Namespace}, &tmpl); err != nil {
		return defaultRetentionTTL
	}

	var pol sprooziv1alpha1.AgentPolicy
	if err := r.Get(ctx, types.NamespacedName{Name: tmpl.Spec.PolicyRef.Name, Namespace: run.Namespace}, &pol); err != nil {
		return defaultRetentionTTL
	}

	ttl := pol.Spec.RetentionTTL.Duration
	if ttl <= 0 {
		return defaultRetentionTTL
	}
	return ttl
}

// resolveRefs fetches the template, policy, and runtime referenced by the run.
// If any reference is missing the run is transitioned to Failed, and all return
// values are nil. The caller should return immediately when template is nil.
func (r *AgentRunReconciler) resolveRefs(
	ctx context.Context,
	run *sprooziv1alpha1.AgentRun,
) (*sprooziv1alpha1.AgentTemplate, *sprooziv1alpha1.AgentPolicy, *sprooziv1alpha1.AgentRuntime, error) {
	var tmpl sprooziv1alpha1.AgentTemplate
	if err := r.Get(ctx, types.NamespacedName{Name: run.Spec.TemplateRef.Name, Namespace: run.Namespace}, &tmpl); err != nil {
		if apierrors.IsNotFound(err) {
			_, ferr := r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseFailed, "TemplateNotFound", "")
			return nil, nil, nil, ferr
		}
		return nil, nil, nil, err
	}

	var pol sprooziv1alpha1.AgentPolicy
	if err := r.Get(ctx, types.NamespacedName{Name: tmpl.Spec.PolicyRef.Name, Namespace: run.Namespace}, &pol); err != nil {
		if apierrors.IsNotFound(err) {
			_, ferr := r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseFailed, "PolicyNotFound", "")
			return nil, nil, nil, ferr
		}
		return nil, nil, nil, err
	}

	var rt sprooziv1alpha1.AgentRuntime
	if err := r.Get(ctx, types.NamespacedName{Name: tmpl.Spec.RuntimeRef.Name, Namespace: run.Namespace}, &rt); err != nil {
		if apierrors.IsNotFound(err) {
			_, ferr := r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseFailed, "RuntimeNotFound", "")
			return nil, nil, nil, ferr
		}
		return nil, nil, nil, err
	}

	return &tmpl, &pol, &rt, nil
}

// transitionPhase validates, applies, and persists a phase change.
// reason must be a CamelCase token suitable for a Condition reason field.
// message is the optional human-readable detail for the status condition.
func (r *AgentRunReconciler) transitionPhase(
	ctx context.Context,
	run *sprooziv1alpha1.AgentRun,
	phase sprooziv1alpha1.AgentRunPhase,
	reason string,
	message string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if !ValidTransition(run.Status.Phase, phase) {
		logger.Info("Ignoring invalid phase transition",
			"name", run.Name,
			"from", run.Status.Phase,
			"to", phase,
		)
		return ctrl.Result{}, nil
	}

	now := metav1.Now()
	run.Status.Phase = phase

	// Set StartedAt when the run is first admitted to execution.
	if phase == sprooziv1alpha1.AgentRunPhaseAdmitted && run.Status.StartedAt == nil {
		run.Status.StartedAt = &now
	}

	// Set CompletedAt when the run enters a terminal phase.
	if IsTerminal(phase) && run.Status.CompletedAt == nil {
		run.Status.CompletedAt = &now
	}

	if reason == "" {
		reason = string(phase)
	}

	condStatus := metav1.ConditionFalse
	switch phase {
	case sprooziv1alpha1.AgentRunPhaseAdmitted,
		sprooziv1alpha1.AgentRunPhaseRunning,
		sprooziv1alpha1.AgentRunPhaseSucceeded:
		condStatus = metav1.ConditionTrue
	}

	apimeta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             condStatus,
		ObservedGeneration: run.Generation,
		Reason:             reason,
		Message:            message,
	})

	if err := r.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("Transitioned AgentRun", "name", run.Name, "phase", phase)
	return ctrl.Result{}, nil
}

// SetupWithManager registers the controller with the manager. MaxConcurrentReconciles
// is pinned to 1 to preserve the single-active-run ordering invariant: concurrent
// reconciliations of two Queued runs could each see no active run and both self-admit.
//
// Three additional watch sources re-enqueue non-terminal AgentRuns when their
// referenced AgentPolicy, AgentTemplate, or AgentRuntime changes, so that a policy
// tightening immediately causes queued runs to be re-evaluated and denied if needed
// (PRD §5: "Policy is evaluated for every active run").
//
// A sandbox Pod watch re-enqueues the owning AgentRun immediately when the Pod
// phase changes, so completed runs are processed without waiting for the deadline
// timer. The watch is filtered to Sproozi-managed Pods only.
func (r *AgentRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := registerIndexes(mgr); err != nil {
		return err
	}

	// An AgentRun event is already enqueued by For. Only wake the next queued
	// run when this event can release the global active slot. This keeps the
	// single-admission invariant while bounding cross-run fanout to one request.
	nextQueued := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		if !IsTerminal(obj.(*sprooziv1alpha1.AgentRun).Status.Phase) && obj.GetDeletionTimestamp().IsZero() {
			return nil
		}
		return r.nextQueuedRequests(ctx)
	})

	// Enqueue only runs that reference the changed template. Policy and runtime
	// changes first resolve the affected templates through indexes, then use the
	// run template index. Terminal runs are excluded because they no longer
	// evaluate live policy.
	runsForTemplate := func(ctx context.Context, namespace, templateName string) []reconcile.Request {
		var list sprooziv1alpha1.AgentRunList
		if err := r.List(ctx, &list, client.InNamespace(namespace), client.MatchingFields{agentRunTemplateIndex: templateName}); err != nil {
			log.FromContext(ctx).Error(err, "Failed to list AgentRuns for template", "namespace", namespace, "template", templateName)
			return nil
		}
		return nonTerminalRequests(list.Items)
	}
	templateRuns := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		return runsForTemplate(ctx, obj.GetNamespace(), obj.GetName())
	})
	templatesFor := func(ctx context.Context, obj client.Object, field string) []reconcile.Request {
		var list sprooziv1alpha1.AgentTemplateList
		if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace()), client.MatchingFields{field: obj.GetName()}); err != nil {
			log.FromContext(ctx).Error(err, "Failed to list AgentTemplates for change", "namespace", obj.GetNamespace(), "name", obj.GetName())
			return nil
		}
		seen := make(map[types.NamespacedName]struct{})
		var reqs []reconcile.Request
		for i := range list.Items {
			for _, req := range runsForTemplate(ctx, list.Items[i].Namespace, list.Items[i].Name) {
				if _, ok := seen[req.NamespacedName]; !ok {
					seen[req.NamespacedName] = struct{}{}
					reqs = append(reqs, req)
				}
			}
		}
		return reqs
	}
	policyRuns := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		return templatesFor(ctx, obj, agentTemplatePolicyIndex)
	})
	runtimeRuns := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		return templatesFor(ctx, obj, agentTemplateRuntimeIndex)
	})

	// sandboxPodMapper maps sandbox Pod events to the owning AgentRun reconcile request.
	// Pods carry RunLabel (run name) and RunNamespaceLabel (run namespace) so the correct
	// namespaced request can be constructed even though Pods live in a different namespace.
	sandboxPodMapper := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
		labels := obj.GetLabels()
		name := labels[k8sresources.RunLabel]
		ns := labels[k8sresources.RunNamespaceLabel]
		if name == "" || ns == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: name, Namespace: ns}}}
	})

	// managedBySproozi filters pod watch events to only Sproozi-owned sandbox Pods,
	// avoiding unnecessary reconcile enqueues for unrelated cluster Pods.
	managedBySproozi := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetLabels()[k8sresources.ManagedByLabel] == k8sresources.ManagedByValue
	})

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(ctrlcontroller.Options{
			// Single concurrent reconciler preserves global active-run ordering.
			MaxConcurrentReconciles: 1,
		}).
		For(&sprooziv1alpha1.AgentRun{}).
		Watches(&sprooziv1alpha1.AgentRun{}, nextQueued).
		Watches(&sprooziv1alpha1.AgentPolicy{}, policyRuns).
		Watches(&sprooziv1alpha1.AgentTemplate{}, templateRuns).
		Watches(&sprooziv1alpha1.AgentRuntime{}, runtimeRuns).
		Watches(&corev1.Pod{}, sandboxPodMapper, builder.WithPredicates(managedBySproozi)).
		Complete(r)
}

func registerIndexes(mgr ctrl.Manager) error {
	indexer := mgr.GetFieldIndexer()
	if err := indexer.IndexField(context.Background(), &sprooziv1alpha1.AgentRun{}, agentRunPhaseIndex, func(obj client.Object) []string {
		return []string{string(obj.(*sprooziv1alpha1.AgentRun).Status.Phase)}
	}); err != nil {
		return err
	}
	if err := indexer.IndexField(context.Background(), &sprooziv1alpha1.AgentRun{}, agentRunTemplateIndex, func(obj client.Object) []string {
		return []string{obj.(*sprooziv1alpha1.AgentRun).Spec.TemplateRef.Name}
	}); err != nil {
		return err
	}
	if err := indexer.IndexField(context.Background(), &sprooziv1alpha1.AgentTemplate{}, agentTemplatePolicyIndex, func(obj client.Object) []string {
		return []string{obj.(*sprooziv1alpha1.AgentTemplate).Spec.PolicyRef.Name}
	}); err != nil {
		return err
	}
	return indexer.IndexField(context.Background(), &sprooziv1alpha1.AgentTemplate{}, agentTemplateRuntimeIndex, func(obj client.Object) []string {
		return []string{obj.(*sprooziv1alpha1.AgentTemplate).Spec.RuntimeRef.Name}
	})
}

func nonTerminalRequests(runs []sprooziv1alpha1.AgentRun) []reconcile.Request {
	requests := make([]reconcile.Request, 0, len(runs))
	for i := range runs {
		if !IsTerminal(runs[i].Status.Phase) {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Name: runs[i].Name, Namespace: runs[i].Namespace}})
		}
	}
	return requests
}

func (r *AgentRunReconciler) nextQueuedRequests(ctx context.Context) []reconcile.Request {
	var list sprooziv1alpha1.AgentRunList
	if err := r.List(ctx, &list, client.MatchingFields{agentRunPhaseIndex: string(sprooziv1alpha1.AgentRunPhaseQueued)}); err != nil {
		log.FromContext(ctx).Error(err, "Failed to find next queued AgentRun")
		return nil
	}
	next := NextQueued(list.Items)
	if next == nil {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Name: next.Name, Namespace: next.Namespace}}}
}
