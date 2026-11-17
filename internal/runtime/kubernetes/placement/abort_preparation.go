package placement

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
	runtimekube "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var _ session.PreparationAborter = (*Placement)(nil)

// abortPreparationWait bounds one AbortPreparation after it takes the
// launch's lock, including its waits for the controller to end a cancelled
// run and for a Pod to stop. It stays under the home's 2-minute wait for an
// abort-preparation request (abortWait in profile), so a remote caller
// normally receives the executor's unavailable answer rather than its own
// timeout, and the next recover resumes the same abort. Time waiting for the lock is not counted,
// so lock wait plus the bound can still exceed the home's timeout. The bound
// does not cover a Pod's termination grace either: it starts before the
// delete, so a container that uses its whole grace (30 seconds by default)
// is seen to stop only by a later attempt.
const abortPreparationWait = 30 * time.Second

// AbortPreparation fences a launch whose preparation never reported an
// incarnation: its run may be queued under a concurrency cap, admitted with
// no Pod yet, failed before its Pod, or prepared with the reply lost. A nil
// result means no writer for this launch exists or can start, and that none
// ever had gateway authority, so there is no usage to report. Anything this
// cannot prove is an error, never success. Every step is idempotent, so a
// retry after a crash or an error finishes the same abort.
//
//  1. A registration that may have had authority (active, stopping, stopped,
//     or cancelled by an abort that did not seal its usage) is refused: it is
//     not a preparation.
//  2. A local tombstone refuses every later Prepare of this launch, so this
//     executor never creates its registration or run afterwards.
//  3. The registration is cancelled by compare-and-swap. It can then never be
//     bound, prepared or activated, and the gateway refuses its run.
//  4. A run never bound to the registration can never get a Pod: the
//     controller creates one only for the run a registration names.
//  5. A bound run is cancelled (spec.cancel). Until the controller has ended
//     it, a reconcile that read the registration before step 3 may still
//     create its Pod, so this waits until the run is gone, terminal or has
//     recorded its sandbox Pod, and only then reads the Pod.
//  6. A Pod that exists is fenced as Abort fences one (ADR 0010): deleted by
//     UID, and fenced only on an observed real termination of the native
//     container, or when it was never scheduled.
func (p *Placement) AbortPreparation(ctx context.Context, l session.Launch) error {
	unlock, err := p.lockOperation(l)
	if err != nil {
		return err
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(ctx, p.abortWait)
	defer cancel()
	// Like Abort, this ends a visit, so it continues from the facts of a
	// contradicted record that hold (settling).
	local, err := p.load(l)
	err = p.forgive(l, err)
	unseen := os.IsNotExist(err)
	if unseen {
		if err = p.validateInput(l, l.ThreadID == "" && l.CheckpointDigest == ""); err != nil {
			return err
		}
		l.Deadline = l.Deadline.UTC()
		local, err = retained{Launch: l}, nil
	}
	if err != nil {
		return err
	}
	r, found, err := p.preparationRegistration(ctx, local)
	if err != nil {
		return err
	}
	if found {
		local, err = local.check(&r)
		if err = p.forgive(l, err); err != nil {
			return err
		}
	}
	if err = p.saveTombstone(&local, unseen, r, found); err != nil || !found {
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
	if r, err = p.fenceCancelled(ctx, &local, r); err != nil {
		return err
	}
	if !unspent(r.Receipt) {
		return spentConflict(r)
	}
	return nil
}

// unspent reports a receipt that sealed no usage, or no receipt at all.
// Success reports no usage, so it is the only receipt an abort of a
// preparation may leave behind.
func unspent(receipt *registry.StopReceipt) bool {
	if receipt == nil {
		return true
	}
	for _, seal := range receipt.Seals {
		if seal != (budget.TrustedUsage{}) {
			return false
		}
	}
	return true
}

func spentConflict(r registry.Record) error {
	return fmt.Errorf("placement: the session registration for AgentRun %s recorded usage, so it was not a preparation; "+
		"only an abort with its incarnation can report that usage: %w", r.IntendedRunName, &profile.Rejection{Code: profile.CodeConflict})
}

// saveTombstone records the launch as aborted, with the registration found
// for it, before anything in the cluster changes. A launch this executor
// never saw also claims its session epoch, as Prepare does, so no other
// allocation prepares that epoch afterwards; an epoch another allocation
// already claimed refuses this one's Prepare anyway.
func (p *Placement) saveTombstone(local *retained, unseen bool, r registry.Record, found bool) error {
	if unseen {
		if err := p.claimEpoch(local.Launch); err != nil && !isConflict(err) {
			return err
		}
	}
	if local.PreparationAborted && (!found || local.Reference == r.Reference) {
		return nil
	}
	local.PreparationAborted = true
	if !found {
		return p.save(*local, nil)
	}
	local.Reference = r.Reference
	return p.save(*local, &r)
}

func isConflict(err error) bool {
	var rejection *profile.Rejection
	return errors.As(err, &rejection) && rejection.Code == profile.CodeConflict
}

// preparationRegistration reads the launch's registration, finding it by run
// name when a crash lost the reference its creation returned, and refuses one
// that is not a preparation. found is false when none ever existed.
//
// Nothing in Sproozi deletes a registration, so one that is gone was deleted
// by an administrator, and its run and Pod may still exist with nothing left
// to fence them by. Both paths answer that as unproven. With the retained
// reference the registration is known to have existed. Found by name, it
// counts as never created only when no run exists at its intended name,
// since the executor creates the run only after the registration. A Pod
// whose run an administrator also deleted is missed: finding it needs a
// list of Pods by run label, which the executor's Role does not grant (ADR
// 0057, section 13).
func (p *Placement) preparationRegistration(ctx context.Context, local retained) (registry.Record, bool, error) {
	var r registry.Record
	var err error
	name := runName(local.Launch)
	if local.Reference.UID != "" {
		r, err = p.registry.Get(ctx, local.Reference)
	} else if r, err = p.registry.Lookup(ctx, name); errors.Is(err, registry.ErrGone) {
		var run api.AgentRun
		runErr := p.config.Kube.Get(ctx, client.ObjectKey{Namespace: p.config.Namespace, Name: name}, &run)
		if apierrors.IsNotFound(runErr) {
			return r, false, nil
		}
		if runErr != nil {
			return r, false, runErr
		}
	}
	if errors.Is(err, registry.ErrGone) {
		return r, false, fmt.Errorf("placement: the session registration %s for AgentRun %s was deleted, so its Pod cannot be fenced; "+
			"an administrator must confirm no Pod labelled %s remains for that run in %s: %w",
			registry.NameForRun(name), name, runtimekube.RunUIDLabel, p.config.AgentNamespace, &profile.Rejection{Code: profile.CodeConflict})
	}
	if err != nil {
		return r, false, semantic(err)
	}
	l := local.Launch
	if r.Harness != l.Harness || r.Principal != p.config.Principal || r.TemplateName != p.config.TemplateName || r.SessionID != l.ExecutionID ||
		r.OperationID != l.OperationID || r.Epoch != l.Epoch || r.IntendedRunName != name {
		return r, false, &profile.Rejection{Code: profile.CodeConflict}
	}
	switch {
	case r.Phase == registry.Creating, r.Phase == registry.Bound, r.Phase == registry.Prepared:
	case r.Phase == registry.Cancelled && (local.PreparationAborted || r.Receipt != nil) && unspent(r.Receipt):
		// This abort's own replay, or a cancellation that already fenced the
		// writer and sealed no usage. A receipt with usage belongs to an
		// abort with the incarnation, which reports it.
	default:
		return r, false, fmt.Errorf("placement: the session registration for AgentRun %s is %s, not a preparation; "+
			"only a stop or an abort with its incarnation can end it: %w", name, r.Phase, &profile.Rejection{Code: profile.CodeConflict})
	}
	return r, true, nil
}

// cancelRun sets spec.cancel on the registration's run unless it is already
// cancelled or terminal, reading the run again when a controller status
// write raced the patch. A run never bound to the registration is still the
// one at its intended name; the controller ends it as cancelled.
func (p *Placement) cancelRun(ctx context.Context, r registry.Record) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var run api.AgentRun
		err := p.config.Kube.Get(ctx, client.ObjectKey{Namespace: p.config.Namespace, Name: r.IntendedRunName}, &run)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if r.RunUID != "" && string(run.UID) != r.RunUID {
			return replacedRun(r, run)
		}
		if run.Spec.Cancel || run.Status.Phase.IsTerminal() {
			return nil
		}
		original := run.DeepCopy()
		run.Spec.Cancel = true
		return p.config.Kube.Patch(ctx, &run, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
	})
}

// replacedRun refuses a run at the registration's intended name that is not
// the run it bound: an administrator deleted and recreated it. The answer is
// a conflict, never a fence.
func replacedRun(r registry.Record, run api.AgentRun) error {
	return fmt.Errorf("placement: AgentRun %s now has UID %s, but its session registration bound UID %s; "+
		"an administrator replaced it, so confirm no Pod labelled %s=%s remains: %w",
		r.IntendedRunName, run.UID, r.RunUID, runtimekube.RunUIDLabel, r.RunUID, &profile.Rejection{Code: profile.CodeConflict})
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
		case err != nil && ctx.Err() == nil:
			return err
		case err == nil && string(run.UID) != r.RunUID:
			return replacedRun(r, run)
		case err == nil && (run.Status.Phase.IsTerminal() || run.Status.Identity.SandboxName != ""):
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
