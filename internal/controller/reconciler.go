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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	policy "github.com/andrewmccall/sproozi/internal/policy/kubernetes"
	k8sresources "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
)

const (
	defaultRetentionTTL = 30 * 24 * time.Hour
	// queuedPollInterval is the fallback re-check for a run left Queued. Runs
	// going terminal or deleted and policy changes wake queued runs sooner.
	queuedPollInterval = 10 * time.Second
	// quotaRetryMin and quotaRetryMax bound the retry after a ResourceQuota
	// rejects a sandbox Pod.
	quotaRetryMin = 5 * time.Second
	quotaRetryMax = time.Minute
	// waitingForCapReason marks a Queued run held back by a cap.
	waitingForCapReason = "WaitingForConcurrencyCap"
	// policyChangedReason fails an active run whose template now names a
	// different policy from the one it was admitted under.
	policyChangedReason = "PolicyChanged"
	// quotaExceededReason marks an active run whose sandbox Pod a
	// ResourceQuota rejected.
	quotaExceededReason = "QuotaExceeded"
	// readyCondition is the AgentRun condition type that carries phase reasons.
	readyCondition            = "Ready"
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
// +kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get

// Sandbox writes (Pods, ServiceAccounts, NetworkPolicies and contract
// ConfigMaps) happen only in sproozi-agents. The Role in config/agent-rbac
// grants them there; markers cannot express it because the default
// kustomization moves every generated object into sproozi-system.

// AgentRunReconciler reconciles AgentRun objects.
type AgentRunReconciler struct {
	client.Client
	// APIReader reads the API server directly. Admission counts active runs
	// with it because the cache can lag this reconciler's own writes.
	APIReader     client.Reader
	SandboxClient k8sresources.SandboxClient
	// MaxActiveRuns caps Admitted and Running runs across the cluster. Zero
	// means no cluster-wide limit.
	MaxActiveRuns int
}

// admission holds the live objects a non-terminal run was last admitted against.
type admission struct {
	template *sprooziv1alpha1.AgentTemplate
	policy   *sprooziv1alpha1.AgentPolicy
	runtime  *sprooziv1alpha1.AgentRuntime
	binding  *registry.Record
	deadline time.Time
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
		return r.transitionPhase(ctx, &run, sprooziv1alpha1.AgentRunPhaseQueued, "Initialized",
			"AgentRun is queued for admission")
	}

	// Terminal runs are only subject to retention TTL deletion.
	if IsTerminal(run.Status.Phase) {
		return r.handleTerminal(ctx, &run)
	}

	// Cancellation is honoured at any non-terminal phase.
	if run.Spec.Cancel {
		return r.transitionPhase(ctx, &run, sprooziv1alpha1.AgentRunPhaseCancelled, "CancelRequested", cancelMessage)
	}

	admitted, result, err := r.resolveAdmission(ctx, &run)
	if admitted == nil {
		return result, err
	}

	switch run.Status.Phase {
	case sprooziv1alpha1.AgentRunPhaseQueued:
		return r.handleQueued(ctx, &run, admitted.policy)
	case sprooziv1alpha1.AgentRunPhaseAdmitted:
		return r.handleAdmitted(ctx, &run, admitted)
	case sprooziv1alpha1.AgentRunPhaseRunning:
		return r.handleRunning(ctx, &run, admitted)
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

// resolveAdmission checks a non-terminal run against its live template, policy,
// runtime and session binding on every reconcile, so a change to any of them
// re-evaluates queued and active runs alike. A nil admission means the run was
// failed, timed out or must wait; the caller returns the result and error.
func (r *AgentRunReconciler) resolveAdmission(ctx context.Context, run *sprooziv1alpha1.AgentRun) (*admission, ctrl.Result, error) {
	logger := log.FromContext(ctx)

	tmpl, pol, rt, err := r.resolveRefs(ctx, run)
	if err != nil || tmpl == nil {
		return nil, ctrl.Result{}, err
	}
	binding, sessionResult, err := r.resolveSession(ctx, run, tmpl)
	if err != nil {
		return nil, ctrl.Result{}, err
	}
	if sessionResult != nil {
		return nil, *sessionResult, nil
	}

	// Reject a runtime that cannot launch before the run is admitted.
	if err := r.SandboxClient.ValidateRuntime(ctx, rt); err != nil {
		if !errors.Is(err, k8sresources.ErrInvalidRuntime) {
			return nil, ctrl.Result{}, err
		}
		logger.Info("AgentRuntime cannot launch AgentRun", "name", run.Name, "runtime", rt.Name, "error", err)
		message := fmt.Sprintf("AgentRuntime %s: %v", rt.Name, err)
		_, err := r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseFailed, "RuntimeInvalid", message)
		return nil, ctrl.Result{}, err
	}

	if denial := policy.Evaluate(pol.Spec, tmpl.Spec, run.Spec, rt.Spec); denial != nil {
		logger.Info("Policy denied AgentRun", "name", run.Name, "phase", run.Status.Phase, "reason", denial.Reason)
		_, err := r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseFailed, string(denial.Reason), denial.Message)
		return nil, ctrl.Result{}, err
	}

	if run.Status.Phase == sprooziv1alpha1.AgentRunPhaseQueued {
		return &admission{template: tmpl, policy: pol, runtime: rt, binding: binding}, ctrl.Result{}, nil
	}

	// transitionPhase records StartedAt with Admitted. If that write was lost,
	// recover it rather than computing the deadline from zero, which would time
	// the run out at once.
	if run.Status.StartedAt == nil {
		now := metav1.Now()
		run.Status.StartedAt = &now
		if err := r.Status().Update(ctx, run); err != nil {
			return nil, ctrl.Result{}, err
		}
		logger.Info("Recovered missing StartedAt on AgentRun", "name", run.Name, "phase", run.Status.Phase)
	}
	deadline := registry.EffectiveDeadline(run, pol, binding)
	if time.Now().After(deadline) {
		logger.Info("AgentRun deadline exceeded", "name", run.Name, "phase", run.Status.Phase)
		_, err := r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseTimedOut, "MaxExecutionDurationExceeded",
			fmt.Sprintf("AgentRun passed its execution deadline %s", deadline.UTC().Format(time.RFC3339)))
		return nil, ctrl.Result{}, err
	}
	return &admission{template: tmpl, policy: pol, runtime: rt, binding: binding, deadline: deadline}, ctrl.Result{}, nil
}

// handleQueued admits the run if a first-in, first-out simulation of the
// cluster-wide queue admits it under the cluster cap and each policy's cap, or
// leaves it Queued with a message naming what is full.
func (r *AgentRunReconciler) handleQueued(ctx context.Context, run *sprooziv1alpha1.AgentRun, pol *sprooziv1alpha1.AgentPolicy) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// A cached list can miss an admission this reconciler has just written,
	// or a run it has not yet seen initialized, and so admit past a cap or
	// let a later run overtake an earlier one. Reconciles are serialized, so a
	// live list reflects every earlier admission decision.
	var allRuns sprooziv1alpha1.AgentRunList
	if err := r.APIReader.List(ctx, &allRuns); err != nil {
		return ctrl.Result{}, err
	}

	state, err := r.admissionState(ctx, allRuns.Items)
	if err != nil {
		return ctrl.Result{}, err
	}
	target := client.ObjectKeyFromObject(run)
	queue, err := r.admissionQueue(ctx, allRuns.Items, target, pol)
	if err != nil {
		return ctrl.Result{}, err
	}
	decision := SimulateAdmission(state, queue, target)
	if !decision.Admit {
		logger.Info("AgentRun stayed queued", "name", run.Name, "message", decision.Message)
		ready := apimeta.FindStatusCondition(run.Status.Conditions, readyCondition)
		if ready == nil || ready.Reason != waitingForCapReason || ready.Message != decision.Message {
			apimeta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
				Type:               readyCondition,
				Status:             metav1.ConditionFalse,
				ObservedGeneration: run.Generation,
				Reason:             waitingForCapReason,
				Message:            decision.Message,
			})
			if err := r.Status().Update(ctx, run); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{RequeueAfter: queuedPollInterval}, nil
	}

	logger.Info("Admitting AgentRun", "name", run.Name, "policy", pol.Name)
	// Record the policy in the same status update that admits the run, so the
	// run counts against it even if its template is re-pointed later.
	run.Status.PolicyName = pol.Name
	return r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseAdmitted, "",
		fmt.Sprintf("AgentRun was admitted under AgentPolicy %s", pol.Name))
}

// admissionState counts active runs cluster-wide and per policy. A run counts
// against the policy recorded at its admission; a run admitted before that
// field existed is resolved through its template's current policyRef.
func (r *AgentRunReconciler) admissionState(ctx context.Context, runs []sprooziv1alpha1.AgentRun) (AdmissionState, error) {
	state := AdmissionState{GlobalCap: r.MaxActiveRuns, PolicyActive: map[types.NamespacedName]int{}}
	for _, active := range ActiveRuns(runs) {
		state.GlobalActive++
		policyName := active.Status.PolicyName
		if policyName == "" {
			var tmpl sprooziv1alpha1.AgentTemplate
			err := r.Get(ctx, types.NamespacedName{Name: active.Spec.TemplateRef.Name, Namespace: active.Namespace}, &tmpl)
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return AdmissionState{}, err
			}
			policyName = tmpl.Spec.PolicyRef.Name
		}
		state.PolicyActive[types.NamespacedName{Namespace: active.Namespace, Name: policyName}]++
	}
	return state, nil
}

// admissionQueue resolves the policy of each queued run up to and including
// the target. The target uses the policy it was just checked against; earlier
// runs whose template or policy is missing, or that are cancelled or being
// deleted, are left unresolved so the simulation skips them.
func (r *AgentRunReconciler) admissionQueue(
	ctx context.Context,
	runs []sprooziv1alpha1.AgentRun,
	target types.NamespacedName,
	targetPolicy *sprooziv1alpha1.AgentPolicy,
) ([]AdmissionCandidate, error) {
	policies := map[types.NamespacedName]*sprooziv1alpha1.AgentPolicy{
		{Namespace: target.Namespace, Name: targetPolicy.Name}: targetPolicy,
	}
	var queue []AdmissionCandidate
	for _, queued := range QueuedRuns(runs) {
		key := client.ObjectKeyFromObject(queued)
		var pol *sprooziv1alpha1.AgentPolicy
		if key == target {
			pol = targetPolicy
		} else if queued.Spec.Cancel || !queued.DeletionTimestamp.IsZero() {
			queue = append(queue, AdmissionCandidate{Run: key})
			continue
		} else {
			var err error
			if pol, err = r.queuedPolicy(ctx, queued, policies); err != nil {
				return nil, err
			}
		}
		candidate := AdmissionCandidate{Run: key, Resolved: pol != nil}
		if pol != nil {
			candidate.Policy = types.NamespacedName{Namespace: queued.Namespace, Name: pol.Name}
			if pol.Spec.MaxActiveRuns != nil {
				candidate.PolicyCap = *pol.Spec.MaxActiveRuns
			}
		}
		queue = append(queue, candidate)
		if key == target {
			break
		}
	}
	return queue, nil
}

// queuedPolicy resolves a queued run's policy through its template, or nil if
// either is missing.
func (r *AgentRunReconciler) queuedPolicy(
	ctx context.Context,
	run *sprooziv1alpha1.AgentRun,
	policies map[types.NamespacedName]*sprooziv1alpha1.AgentPolicy,
) (*sprooziv1alpha1.AgentPolicy, error) {
	var tmpl sprooziv1alpha1.AgentTemplate
	if err := r.Get(ctx, types.NamespacedName{Name: run.Spec.TemplateRef.Name, Namespace: run.Namespace}, &tmpl); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	key := types.NamespacedName{Namespace: run.Namespace, Name: tmpl.Spec.PolicyRef.Name}
	if pol, ok := policies[key]; ok {
		return pol, nil
	}
	var pol sprooziv1alpha1.AgentPolicy
	if err := r.Get(ctx, key, &pol); err != nil {
		if apierrors.IsNotFound(err) {
			policies[key] = nil
			return nil, nil
		}
		return nil, err
	}
	policies[key] = &pol
	return &pol, nil
}

// handleAdmitted provisions the per-run identity resources (ServiceAccount and
// NetworkPolicy) and transitions the run to Running once identity is in place.
// Kubernetes API authorization is deliberately owned by the shared gateway; the
// workload ServiceAccount is an immutable run identity only and is never bound
// to namespace Roles.
func (r *AgentRunReconciler) handleAdmitted(ctx context.Context, run *sprooziv1alpha1.AgentRun, admitted *admission) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if run.Status.Identity.ServiceAccountName == "" {
		saName, err := k8sresources.EnsureServiceAccount(ctx, r.Client, run)
		if err != nil {
			return ctrl.Result{}, err
		}
		if err := k8sresources.EnsureNetworkPolicy(ctx, r.Client, run, admitted.policy); err != nil {
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

	return r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseRunning, "",
		fmt.Sprintf("Run identity ServiceAccount %s is provisioned", run.Status.Identity.ServiceAccountName))
}

// handleRunning creates the Sandbox Pod, monitors its lifecycle, and transitions
// the run to Succeeded or Failed based on sandbox phase.
func (r *AgentRunReconciler) handleRunning(ctx context.Context, run *sprooziv1alpha1.AgentRun, admitted *admission) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	if run.Status.Identity.ServiceAccountName != "" {
		if err := k8sresources.EnsureNetworkPolicy(ctx, r.Client, run, admitted.policy); err != nil {
			return ctrl.Result{}, err
		}
	}
	if err := k8sresources.EnsureAgentContract(ctx, r.Client, run, admitted.template, admitted.runtime); err != nil {
		return ctrl.Result{}, err
	}

	sandboxName, err := r.ensureSandbox(ctx, run, admitted.runtime, admitted.binding)
	if k8sresources.IsQuotaExceeded(err) {
		return r.waitForQuota(ctx, run, err, admitted.deadline)
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	sandboxStatus, err := r.SandboxClient.GetStatus(ctx, sandboxName)
	if err != nil {
		return ctrl.Result{}, err
	}

	switch sandboxStatus {
	case k8sresources.SandboxStatusSucceeded, k8sresources.SandboxStatusFailed:
		return r.processSandboxExit(ctx, run, sandboxName)

	case k8sresources.SandboxStatusAbsent:
		logger.Info("Sandbox terminated abnormally", "name", run.Name, "sandboxStatus", sandboxStatus)
		return r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseFailed, "SandboxGone",
			fmt.Sprintf("Sandbox Pod %s disappeared before the agent container exited", sandboxName))
	}

	// Pending, Running or Unknown: wake at the deadline; Pod events wake sooner.
	return ctrl.Result{RequeueAfter: time.Until(admitted.deadline)}, nil
}

// waitForQuota keeps a run whose sandbox Pod a ResourceQuota rejected active,
// with Ready reason QuotaExceeded, and retries with a backoff that grows with
// the time since the first rejection. The condition is written only when it
// first appears, so the write does not trigger a reconcile per retry. The
// run's deadline still applies on each reconcile.
func (r *AgentRunReconciler) waitForQuota(ctx context.Context, run *sprooziv1alpha1.AgentRun, quotaErr error, deadline time.Time) (ctrl.Result, error) {
	ready := apimeta.FindStatusCondition(run.Status.Conditions, readyCondition)
	if ready == nil || ready.Reason != quotaExceededReason {
		log.FromContext(ctx).Info("ResourceQuota rejected sandbox Pod", "name", run.Name, "error", quotaErr.Error())
		apimeta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{
			Type:               readyCondition,
			Status:             metav1.ConditionFalse,
			ObservedGeneration: run.Generation,
			Reason:             quotaExceededReason,
			Message:            fmt.Sprintf("A ResourceQuota rejected the sandbox Pod; retrying until the run deadline: %v", quotaErr),
		})
		if err := r.Status().Update(ctx, run); err != nil {
			return ctrl.Result{}, err
		}
		ready = apimeta.FindStatusCondition(run.Status.Conditions, readyCondition)
	}
	wait := min(max(time.Since(ready.LastTransitionTime.Time), quotaRetryMin), quotaRetryMax)
	wait = min(wait, max(time.Until(deadline), time.Second))
	return ctrl.Result{RequeueAfter: wait}, nil
}

// handleTerminal revokes any provisioned identity resources and then deletes
// the run once its retention TTL has elapsed. It does NOT call resolveRefs to
// avoid mutating terminal runs if their referenced resources have been deleted.
func (r *AgentRunReconciler) handleTerminal(ctx context.Context, run *sprooziv1alpha1.AgentRun) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	complete, err := r.ReconcileRelease(ctx, run)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !complete {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if run.Status.Identity.ServiceAccountName != "" || run.Status.Identity.SandboxName != "" || len(run.Status.Identity.GrantedNamespaces) != 0 {
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
	// A Pod create may succeed before the status write. Recover only the
	// deterministic object bearing this immutable Run identity.
	if run.UID == "" {
		return false, errors.New("cannot release AgentRun without UID")
	}
	if run.Status.Identity.SandboxName == "" {
		var pod corev1.Pod
		err := r.Get(ctx, client.ObjectKey{Namespace: k8sresources.AgentsNamespace, Name: k8sresources.RunName(run.UID)}, &pod)
		if err != nil && !apierrors.IsNotFound(err) {
			return false, err
		}
		if err == nil {
			if pod.Labels[k8sresources.ManagedByLabel] != k8sresources.ManagedByValue || pod.Labels[k8sresources.RunUIDLabel] != string(run.UID) {
				return false, errors.New("cannot recover sandbox with different Run identity")
			}
			run.Status.Identity.SandboxName = pod.Name
		}
	}
	if run.Status.Identity.SandboxName != "" {
		if err := r.SandboxClient.DeleteForRun(ctx, run.Status.Identity.SandboxName, string(run.UID)); err != nil {
			return false, err
		}
		status, err := r.SandboxClient.GetStatus(ctx, run.Status.Identity.SandboxName)
		if err != nil {
			return false, err
		}
		if status != k8sresources.SandboxStatusAbsent && status != k8sresources.SandboxStatusSucceeded && status != k8sresources.SandboxStatusFailed {
			return false, nil
		}
	}
	// Terminal phase is the gateway revocation point: gateway lookups reject it
	// before any disposable identity is removed.
	if err := k8sresources.RevokeAgentContract(ctx, r.Client, run, string(run.UID)); err != nil {
		return false, err
	}
	saName := run.Status.Identity.ServiceAccountName
	if saName == "" {
		saName = k8sresources.RunName(run.UID)
	}
	if err := k8sresources.RevokeNetworkPolicy(ctx, r.Client, saName, string(run.UID)); err != nil {
		return false, err
	}
	if err := k8sresources.RevokeServiceAccount(ctx, r.Client, saName, string(run.UID)); err != nil {
		return false, err
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
			_, ferr := r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseFailed, "TemplateNotFound",
				fmt.Sprintf("AgentTemplate %s not found in namespace %s", run.Spec.TemplateRef.Name, run.Namespace))
			return nil, nil, nil, ferr
		}
		return nil, nil, nil, err
	}

	// An active run counts against the policy it was admitted under. If its
	// template now names another policy, that policy's cap would not limit it,
	// so fail closed as for a deleted policy. The gateway denies the same runs.
	if registry.AdmittedPolicyChanged(run, &tmpl) {
		_, ferr := r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseFailed, policyChangedReason,
			registry.PolicyChangedMessage(run, &tmpl))
		return nil, nil, nil, ferr
	}

	var pol sprooziv1alpha1.AgentPolicy
	if err := r.Get(ctx, types.NamespacedName{Name: tmpl.Spec.PolicyRef.Name, Namespace: run.Namespace}, &pol); err != nil {
		if apierrors.IsNotFound(err) {
			_, ferr := r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseFailed, "PolicyNotFound",
				fmt.Sprintf("AgentPolicy %s referenced by AgentTemplate %s not found in namespace %s",
					tmpl.Spec.PolicyRef.Name, tmpl.Name, run.Namespace))
			return nil, nil, nil, ferr
		}
		return nil, nil, nil, err
	}

	var rt sprooziv1alpha1.AgentRuntime
	if err := r.Get(ctx, types.NamespacedName{Name: tmpl.Spec.RuntimeRef.Name, Namespace: run.Namespace}, &rt); err != nil {
		if apierrors.IsNotFound(err) {
			_, ferr := r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseFailed, "RuntimeNotFound",
				fmt.Sprintf("AgentRuntime %s referenced by AgentTemplate %s not found in namespace %s",
					tmpl.Spec.RuntimeRef.Name, tmpl.Name, run.Namespace))
			return nil, nil, nil, ferr
		}
		return nil, nil, nil, err
	}

	return &tmpl, &pol, &rt, nil
}

// cancelMessage is shared by every path that honours spec.cancel.
const cancelMessage = "AgentRun was cancelled because spec.cancel is true"

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
	from := run.Status.Phase
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
		Type:               readyCondition,
		Status:             condStatus,
		ObservedGeneration: run.Generation,
		Reason:             reason,
		Message:            message,
	})

	if err := r.Status().Update(ctx, run); err != nil {
		return ctrl.Result{}, err
	}

	r.traceTransition(ctx, run, from, reason)
	logger.Info("Transitioned AgentRun", "name", run.Name, "phase", phase)
	return ctrl.Result{}, nil
}

// SetupWithManager registers the controller with the manager. MaxConcurrentReconciles
// is pinned to 1 so admission counts stay correct: concurrent reconciliations of
// two Queued runs could each see the same free capacity and both self-admit past
// a cap.
//
// Three additional watch sources re-enqueue non-terminal AgentRuns when their
// referenced AgentPolicy, AgentTemplate, or AgentRuntime changes, so that a policy
// tightening immediately causes queued runs to be re-evaluated and denied if needed
// (PRD §5: "Policy is evaluated for every active run"). A policy change also wakes
// every queued run, because a changed cap can admit them.
//
// A sandbox Pod watch re-enqueues the owning AgentRun immediately when the Pod
// phase changes, so completed runs are processed without waiting for the deadline
// timer. The watch is filtered to Sproozi-managed Pods only.
func (r *AgentRunReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := registerIndexes(mgr); err != nil {
		return err
	}

	// An AgentRun event is already enqueued by For. When this event can free
	// capacity (the run is terminal or being deleted), wake every queued run:
	// with per-policy caps the run that can now be admitted need not be the
	// queue head.
	queuedOnRelease := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, obj client.Object) []reconcile.Request {
		if !IsTerminal(obj.(*sprooziv1alpha1.AgentRun).Status.Phase) && obj.GetDeletionTimestamp().IsZero() {
			return nil
		}
		return r.queuedRequests(ctx)
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
		return mergeRequests(templatesFor(ctx, obj, agentTemplatePolicyIndex), r.queuedRequests(ctx))
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
			// One reconciler at a time keeps admission counts and FIFO order
			// correct against the live run list.
			MaxConcurrentReconciles: 1,
		}).
		For(&sprooziv1alpha1.AgentRun{}).
		Watches(&sprooziv1alpha1.AgentRun{}, queuedOnRelease).
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

// queuedRequests returns every Queued run, read from the cache. Each one
// re-checks its own admission against the live list.
func (r *AgentRunReconciler) queuedRequests(ctx context.Context) []reconcile.Request {
	var list sprooziv1alpha1.AgentRunList
	if err := r.List(ctx, &list, client.MatchingFields{agentRunPhaseIndex: string(sprooziv1alpha1.AgentRunPhaseQueued)}); err != nil {
		log.FromContext(ctx).Error(err, "Failed to list queued AgentRuns")
		return nil
	}
	return nonTerminalRequests(list.Items)
}

// mergeRequests joins request lists without duplicates.
func mergeRequests(lists ...[]reconcile.Request) []reconcile.Request {
	seen := make(map[types.NamespacedName]struct{})
	var merged []reconcile.Request
	for _, list := range lists {
		for _, req := range list {
			if _, ok := seen[req.NamespacedName]; !ok {
				seen[req.NamespacedName] = struct{}{}
				merged = append(merged, req)
			}
		}
	}
	return merged
}
