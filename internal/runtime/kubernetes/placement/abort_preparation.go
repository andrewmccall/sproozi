package placement

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	runtimekube "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ session.PreparationAborter = (*Placement)(nil)

// AbortPreparation fences a launch whose preparation never reported an
// incarnation: its run may be queued under a concurrency cap, admitted with
// no Pod yet, failed before its Pod, or prepared with the reply lost. A nil
// result means no writer for this launch exists and none can start; anything
// this cannot prove is an error, never success. Every step is idempotent, so
// a retry after a crash or an error finishes the same abort.
//
//  1. A local tombstone refuses every later Prepare of this launch, so this
//     executor never creates its registration or run afterwards.
//  2. The registration is cancelled by compare-and-swap. It can then never be
//     bound, prepared or activated, and the gateway refuses its run.
//  3. A run never bound to the registration can never get a Pod: the
//     controller creates one only for the run a registration names.
//  4. A bound run is cancelled (spec.cancel). Until the controller has ended
//     it, a reconcile that read the registration before step 2 may still
//     create its Pod, so this waits until the run is gone, terminal or has
//     recorded its sandbox Pod, and only then reads the Pod.
//  5. A Pod that exists is fenced as Abort fences one (ADR 0010): deleted by
//     UID, and fenced only on an observed real termination of the native
//     container, or when it was never scheduled.
func (p *Placement) AbortPreparation(ctx context.Context, l session.Launch) error {
	unlock, err := p.lockOperation(l)
	if err != nil {
		return err
	}
	defer unlock()
	local, err := p.abortedIntent(l)
	if err != nil {
		return err
	}
	r, found, err := p.preparationRegistration(ctx, &local)
	if err != nil || !found {
		return err
	}
	if r, err = p.registry.Cancel(ctx, r.Reference); err != nil {
		return semantic(err)
	}
	if err = p.cancelRun(ctx, r); err != nil {
		return err
	}
	if r.RunUID == "" {
		return nil
	}
	if r.PodUID == "" {
		if err = p.awaitSandboxSettled(ctx, r); err != nil {
			return err
		}
		pod, err := p.sandbox(ctx, r)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if r, err = p.registry.BindPod(ctx, r.Reference, string(pod.UID)); err != nil {
			return semantic(err)
		}
	}
	_, err = p.fenceCancelled(ctx, &local, r)
	return err
}

// abortedIntent loads or creates the launch's local record and saves it as
// aborted before anything else changes.
func (p *Placement) abortedIntent(l session.Launch) (retained, error) {
	local, err := p.load(l)
	if os.IsNotExist(err) {
		if err = p.validateInput(l, l.ThreadID == "" && l.CheckpointDigest == ""); err != nil {
			return local, err
		}
		l.Deadline = l.Deadline.UTC()
		local, err = retained{Launch: l}, nil
	}
	if err != nil || local.PreparationAborted {
		return local, err
	}
	local.PreparationAborted = true
	return local, p.save(local)
}

// preparationRegistration reads the launch's registration, finding it by run
// name when a crash lost the reference its creation returned. found is false
// when none exists.
func (p *Placement) preparationRegistration(ctx context.Context, local *retained) (registry.Record, bool, error) {
	var r registry.Record
	var err error
	if local.Reference.UID != "" {
		r, err = p.registry.Get(ctx, local.Reference)
	} else {
		r, err = p.registry.Lookup(ctx, runName(local.Launch))
		if errors.Is(err, registry.ErrGone) {
			return r, false, nil
		}
	}
	if err != nil {
		return r, false, semantic(err)
	}
	l := local.Launch
	if r.Harness != l.Harness || r.Principal != p.config.Principal || r.TemplateName != p.config.TemplateName || r.SessionID != l.ExecutionID ||
		r.OperationID != l.OperationID || r.Epoch != l.Epoch || r.IntendedRunName != runName(l) {
		return r, false, &profile.Rejection{Code: profile.CodeConflict}
	}
	if local.Reference.UID == "" {
		local.Reference = r.Reference
		if err = p.save(*local); err != nil {
			return r, false, err
		}
	}
	return r, true, nil
}

// cancelRun sets spec.cancel on the registration's run unless it is already
// cancelled or terminal. A run never bound to the registration is still the
// one at its intended name; the controller ends it as cancelled.
func (p *Placement) cancelRun(ctx context.Context, r registry.Record) error {
	var run api.AgentRun
	err := p.config.Kube.Get(ctx, client.ObjectKey{Namespace: p.config.Namespace, Name: r.IntendedRunName}, &run)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.RunUID != "" && string(run.UID) != r.RunUID {
		return &profile.Rejection{Code: profile.CodeConflict}
	}
	if run.Spec.Cancel || run.Status.Phase.IsTerminal() {
		return nil
	}
	original := run.DeepCopy()
	run.Spec.Cancel = true
	return p.config.Kube.Patch(ctx, &run, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
}

// awaitSandboxSettled waits until the controller can no longer create the
// run's Pod: the run is gone, terminal, or has recorded its sandbox Pod. The
// controller creates that Pod only while a live read shows the run neither
// cancelled, terminal nor deleted, and reconciles of one run never overlap,
// so after any of these a Pod that will ever exist already does.
func (p *Placement) awaitSandboxSettled(ctx context.Context, r registry.Record) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var run api.AgentRun
		err := p.config.Kube.Get(ctx, client.ObjectKey{Namespace: p.config.Namespace, Name: r.IntendedRunName}, &run)
		switch {
		case apierrors.IsNotFound(err):
			return nil
		case err != nil:
			return err
		case string(run.UID) != r.RunUID:
			return &profile.Rejection{Code: profile.CodeConflict}
		case run.Status.Phase.IsTerminal() || run.Status.Identity.SandboxName != "":
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("placement: AgentRun %s is cancelled but the controller has not ended it, so its Pod may still be created: %w",
				r.IntendedRunName, ctx.Err())
		case <-ticker.C:
		}
	}
}

// sandbox reads the bound run's Pod by its fixed name, whatever its UID.
func (p *Placement) sandbox(ctx context.Context, r registry.Record) (*corev1.Pod, error) {
	var pod corev1.Pod
	if err := p.config.Kube.Get(ctx, client.ObjectKey{Namespace: p.config.AgentNamespace, Name: runtimekube.RunName(types.UID(r.RunUID))}, &pod); err != nil {
		return nil, err
	}
	if pod.Labels[runtimekube.RunUIDLabel] != r.RunUID || pod.Labels[runtimekube.RunNamespaceLabel] != p.config.Namespace || pod.UID == "" {
		return nil, &profile.Rejection{Code: profile.CodeConflict}
	}
	return &pod, nil
}

// neverScheduled reports a Pod no kubelet ever ran: being deleted, assigned
// to no node and with no container ever started. The API server refuses to
// bind a Pod that is being deleted to a node, so it never will run.
func neverScheduled(pod *corev1.Pod) bool {
	if pod.Spec.NodeName != "" || pod.DeletionTimestamp.IsZero() {
		return false
	}
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses, pod.Status.EphemeralContainerStatuses} {
		for _, c := range statuses {
			if c.ContainerID != "" {
				return false
			}
		}
	}
	return true
}
