package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ManagedTemplateAnnotation is set only on an administrator-owned template.
// A Run annotation cannot opt an execution out of required registration.
const ManagedTemplateAnnotation = "sessions.sproozi.com/managed"

var ErrBindingPending = errors.New("session registration awaits exact Run binding")

func InstructionsDigest(instructions string) string {
	sum := sha256.Sum256([]byte(instructions))
	return hex.EncodeToString(sum[:])
}

// ResolveRun reads trusted registration for managed templates. A nil record
// means an ordinary template, never a missing managed record. The service alone
// writes records in the Run namespace; checkpoints and Run metadata cannot grant
// this binding. Fresh policy evaluation remains the controller's responsibility.
func ResolveRun(ctx context.Context, kube client.Client, run *api.AgentRun, template *api.AgentTemplate) (*Record, error) {
	managed := template.Annotations[ManagedTemplateAnnotation]
	if managed == "" {
		return nil, nil
	}
	if managed != enabledValue || template.Namespace != run.Namespace || template.Name != run.Spec.TemplateRef.Name {
		return nil, ErrConflict
	}
	var cm corev1.ConfigMap
	if err := kube.Get(ctx, client.ObjectKey{Namespace: run.Namespace, Name: NameForRun(run.Name)}, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrGone
		}
		return nil, err
	}
	record, err := read(&cm)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	if record.IntendedRunName != run.Name || record.TemplateName != template.Name ||
		record.InstructionDigest != InstructionsDigest(template.Spec.Instructions) {
		return nil, ErrConflict
	}
	if !record.OriginalDeadline.After(time.Now()) {
		return nil, ErrExpired
	}
	if record.Phase == Creating && record.RunUID == "" {
		return nil, ErrBindingPending
	}
	if record.RunUID != string(run.UID) || record.Phase == Cancelled || record.Phase == Stopped {
		return nil, ErrConflict
	}
	if len(record.Ceilings) != len(run.Spec.Capabilities) {
		return nil, ErrConflict
	}
	for capability := range record.Ceilings {
		if !slices.Contains(run.Spec.Capabilities, capability) {
			return nil, ErrConflict
		}
	}
	return &record, nil
}

// EffectiveDeadline never extends the session deadline after queueing, restore,
// or a policy change. Ordinary Runs retain their current StartedAt calculation.
func EffectiveDeadline(run *api.AgentRun, policy *api.AgentPolicy, record *Record) time.Time {
	var deadline time.Time
	if run.Status.StartedAt != nil {
		deadline = run.Status.StartedAt.Add(policy.Spec.MaxExecutionDuration.Duration)
	}
	if record != nil && (deadline.IsZero() || record.OriginalDeadline.Before(deadline)) {
		deadline = record.OriginalDeadline
	}
	return deadline
}

// RequireActive verifies the exact live worker container in addition to the
// retained activation record. A Running capture sidecar cannot authorize an
// already terminated native container. Callers also verify live policy and Run.
func RequireActive(ctx context.Context, kube client.Client, run *api.AgentRun, record *Record, agentNamespace string) error {
	if record == nil {
		return nil
	}
	if record.Phase != Active || record.RunUID != string(run.UID) || !record.OriginalDeadline.After(time.Now()) ||
		run.Status.Identity.SandboxName == "" || record.PodUID == "" {
		return ErrConflict
	}
	var pod corev1.Pod
	if err := kube.Get(ctx, client.ObjectKey{Namespace: agentNamespace, Name: run.Status.Identity.SandboxName}, &pod); err != nil {
		return err
	}
	if string(pod.UID) != record.PodUID || !pod.DeletionTimestamp.IsZero() || pod.Status.Phase != corev1.PodRunning ||
		pod.Spec.ServiceAccountName != run.Status.Identity.ServiceAccountName || pod.Labels["sproozi.com/agentrun-uid"] != record.RunUID {
		return ErrConflict
	}
	for _, container := range pod.Status.ContainerStatuses {
		if container.Name == "agent" && container.State.Running != nil && container.ContainerID != "" {
			return nil
		}
	}
	return fmt.Errorf("session native container is not running")
}
