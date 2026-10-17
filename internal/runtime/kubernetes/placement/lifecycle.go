package placement

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strconv"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/harness"
	runtimekube "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var errWriterFenceUnknown = errors.New("placement: writer fence remains unknown")

const containerStatusUnknown = "ContainerStatusUnknown"

func (p *Placement) retained(ctx context.Context, l session.Launch) (retained, registry.Record, error) {
	local, err := p.load(l)
	if err != nil {
		return local, registry.Record{}, err
	}
	if local.Reference.UID == "" {
		ceilings, err := p.liveProfileInput(ctx, l, local.Initial != nil)
		if err != nil {
			return local, registry.Record{}, err
		}
		registration := p.registration(l, ceilings)
		registration.WorkspaceDigest = local.WorkspaceDigest
		r, err := p.registry.Create(ctx, registration)
		if err != nil {
			return local, r, semantic(err)
		}
		local.Reference = r.Reference
		return local, r, p.save(local)
	}
	r, err := p.registry.Get(ctx, local.Reference)
	if err != nil {
		return local, r, semantic(err)
	}
	if local.Initial != nil && r.NativeUUID != "" && local.Launch.ThreadID == "" {
		local.Launch.ThreadID = r.NativeUUID
		if err = p.save(local); err != nil {
			return local, r, err
		}
	}
	l = local.Launch
	if r.Harness != p.config.Harness || l.Harness != r.Harness || r.Principal != p.config.Principal || r.TemplateName != p.config.TemplateName || r.Epoch != l.Epoch || r.IntendedRunName != runName(l) || r.OperationID != l.OperationID || r.SessionID != l.SessionID || r.WorkspaceDigest != local.WorkspaceDigest || r.Task != l.Task || r.NativeUUID != l.ThreadID || r.CheckpointDigest != l.CheckpointDigest || r.InstructionDigest != l.InstructionDigest || !r.OriginalDeadline.Equal(l.Deadline) || !slices.Equal(r.IncludeIgnored, l.IncludeIgnored) || !ceilingsMatch(l, r.Ceilings) {
		return local, r, &profile.Rejection{Code: profile.CodeConflict}
	}
	return local, r, nil
}

func ceilingsMatch(l session.Launch, ceilings map[api.CapabilityKind]api.EndpointBudget) bool {
	if len(l.Allowances) != len(ceilings) {
		return false
	}
	for name, a := range l.Allowances {
		c, ok := ceilings[api.CapabilityKind(name)]
		if !ok || !a.Units.Bounded || c.MaxUnits != a.Units.Value || c.MaxCostMicros != a.CostMicros.Value {
			return false
		}
	}
	return true
}
func (p *Placement) pod(ctx context.Context, r registry.Record) (*corev1.Pod, error) {
	var run api.AgentRun
	if err := p.config.Kube.Get(ctx, client.ObjectKey{Namespace: p.config.Namespace, Name: r.IntendedRunName}, &run); err != nil {
		return nil, err
	}
	if string(run.UID) != r.RunUID || run.Status.Identity.SandboxName == "" {
		return nil, errors.New("placement: exact admitted run unavailable")
	}
	var pod corev1.Pod
	if err := p.config.Kube.Get(ctx, client.ObjectKey{Namespace: p.config.AgentNamespace, Name: run.Status.Identity.SandboxName}, &pod); err != nil {
		return nil, err
	}
	if r.PodUID != "" && string(pod.UID) != r.PodUID || pod.Labels[runtimekube.RunUIDLabel] != r.RunUID || pod.Labels[runtimekube.RunNamespaceLabel] != p.config.Namespace || pod.Spec.ServiceAccountName != run.Status.Identity.ServiceAccountName || string(pod.UID) == "" {
		return nil, &profile.Rejection{Code: profile.CodeConflict}
	}
	return &pod, nil
}
func running(pod *corev1.Pod, name string) bool {
	for _, c := range pod.Status.ContainerStatuses {
		if c.Name == name && c.State.Running != nil && c.ContainerID != "" {
			return true
		}
	}
	return false
}

func (p *Placement) activePod(ctx context.Context, r registry.Record) (*corev1.Pod, error) {
	var run api.AgentRun
	if err := p.config.Kube.Get(ctx, client.ObjectKey{Namespace: p.config.Namespace, Name: r.IntendedRunName}, &run); err != nil {
		return nil, err
	}
	if run.Spec.Cancel || run.Status.Phase != api.AgentRunPhaseRunning {
		return nil, &profile.Rejection{Code: profile.CodeConflict}
	}
	if err := registry.RequireActive(ctx, p.config.Kube, &run, &r, p.config.AgentNamespace); err != nil {
		return nil, semantic(err)
	}
	return p.pod(ctx, r)
}
func terminated(pod *corev1.Pod) bool {
	for _, c := range pod.Status.ContainerStatuses {
		state := c.State.Terminated
		if c.Name == nativeContainer && state != nil && c.ContainerID != "" &&
			state.ContainerID == c.ContainerID && !state.FinishedAt.IsZero() && state.Reason != containerStatusUnknown {
			return true
		}
	}
	return false
}
func (p *Placement) waitPod(ctx context.Context, r registry.Record, wantRunning bool) (*corev1.Pod, error) {
	deadline := r.OriginalDeadline
	if wantRunning && r.Task == "" {
		deadline = minTime(deadline, time.Now().Add(2*time.Minute))
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		pod, err := p.pod(ctx, r)
		if err == nil {
			if wantRunning && running(pod, nativeContainer) && running(pod, runtimekube.CaptureContainer) && pod.DeletionTimestamp.IsZero() {
				return pod, nil
			}
			if !wantRunning && terminated(pod) {
				return pod, nil
			}
		}
		if err != nil && !apierrors.IsNotFound(err) && r.PodUID != "" {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}
func (p *Placement) Activate(ctx context.Context, l session.Launch, i session.Incarnation) error {
	unlock, err := p.lockOperation(l)
	if err != nil {
		return err
	}
	defer unlock()
	local, r, err := p.retained(ctx, l)
	if err != nil {
		return err
	}
	approved := l
	if local.Initial != nil {
		approved = *local.Initial
	}
	if _, err = p.liveProfileInput(ctx, approved, local.Initial != nil); err != nil {
		return err
	}
	if inc(r) != i {
		return &profile.Rejection{Code: profile.CodeConflict}
	}
	if r.Phase != registry.Prepared && r.Phase != registry.Active {
		return &profile.Rejection{Code: profile.CodeConflict}
	}
	pod, err := p.pod(ctx, r)
	if err != nil {
		return err
	}
	if !running(pod, nativeContainer) || !pod.DeletionTimestamp.IsZero() {
		return errors.New("placement: inert worker unavailable")
	}
	// This record change enables only the already bound worker, never a new Run.
	if _, err = p.registry.Activate(ctx, r.Reference); err != nil {
		return semantic(err)
	}
	output, err := p.native(pod)(ctx, harness.NativeActivate)
	if err != nil {
		return err
	}
	if local.Initial != nil {
		_, err = harness.DecodeInitialNativeReply(output, harness.NativeActivate, harness.NativeActive, r.NativeUUID)
		return err
	}
	_, err = harness.DecodeNativeReply(output, harness.NativeActivate, harness.NativeActive, local.Launch.ThreadID)
	return err
}
func (p *Placement) Inspect(ctx context.Context, l session.Launch) (session.Observation, error) {
	unlock, err := p.lockOperation(l)
	if err != nil {
		return session.Observation{}, err
	}
	defer unlock()
	local, r, err := p.retained(ctx, l)
	if os.IsNotExist(err) {
		var cm corev1.ConfigMap
		lookupErr := p.config.Kube.Get(ctx, client.ObjectKey{Namespace: p.config.Namespace, Name: registry.NameForRun(runName(l))}, &cm)
		if apierrors.IsNotFound(lookupErr) {
			return session.Observation{Launch: l, State: session.PlacementAbsent}, nil
		}
		if lookupErr != nil {
			return session.Observation{}, lookupErr
		}
		return session.Observation{Launch: l, State: session.PlacementUnknown}, nil
	}
	if err != nil {
		return session.Observation{}, err
	}
	l = local.Launch
	o := session.Observation{Launch: l, Incarnation: inc(r), State: session.PlacementUnknown}
	switch r.Phase {
	case registry.Creating:
		// Observation never creates a Run or invokes a restore helper. Recovery
		// explicitly calls Prepare again; cancellation needs an observed identity.
		o.State = session.PlacementPreparing
	case registry.Bound:
		if r.PodUID == "" {
			o.Incarnation = session.Incarnation{}
		}
		pod, err := p.pod(ctx, r)
		if apierrors.IsNotFound(err) {
			o.State = session.PlacementPreparing
			return o, nil
		}
		if err != nil {
			return o, err
		}
		r, err = p.registry.BindPod(ctx, r.Reference, string(pod.UID))
		if err != nil {
			return o, semantic(err)
		}
		o.Incarnation, o.State = inc(r), session.PlacementPreparing
	case registry.Prepared:
		pod, err := p.pod(ctx, r)
		if err != nil {
			return o, err
		}
		if !running(pod, nativeContainer) {
			return o, nil
		}
		data, err := p.native(pod)(ctx, harness.NativeStatus)
		if err != nil {
			return o, err
		}
		if _, err := harness.DecodeNativeReply(data, harness.NativeStatus, harness.NativePrepared, l.ThreadID); err == nil {
			o.State = session.PlacementPrepared
		}
	case registry.Active:
		pod, err := p.activePod(ctx, r)
		if err != nil {
			return o, err
		}
		if !running(pod, nativeContainer) || !pod.DeletionTimestamp.IsZero() {
			return o, nil
		}
		data, err := p.native(pod)(ctx, harness.NativeStatus)
		if err != nil {
			return o, err
		}
		_, activeErr := harness.DecodeNativeReply(data, harness.NativeStatus, harness.NativeActive, l.ThreadID)
		if local.Initial != nil {
			_, activeErr = harness.DecodeInitialNativeReply(data, harness.NativeStatus, harness.NativeActive, r.NativeUUID)
		}
		if activeErr == nil {
			o.State = session.PlacementActive
		} else if _, err := harness.DecodeNativeReply(data, harness.NativeStatus, harness.NativePrepared, l.ThreadID); err == nil {
			o.State = session.PlacementPrepared
		}
	case registry.Stopped, registry.Cancelled:
		return p.inspectStopped(ctx, local, r, o)
	case registry.Stopping: // Stop replay seals/captures the same retained execution.
	}
	return o, nil
}

// Both terminal registry phases still require trusted physical evidence. Only a
// graceful stop also has a newly captured checkpoint; cancellation may retain
// unknown usage or the last previously committed artifact.
func (p *Placement) inspectStopped(ctx context.Context, local retained, r registry.Record, o session.Observation) (session.Observation, error) {
	fenced, err := p.physicalFence(ctx, &local, r)
	if err != nil {
		return o, err
	}
	if !fenced {
		if r.Phase == registry.Stopped {
			return o, errWriterFenceUnknown
		}
		return o, nil
	}
	stopped := cancelledStopped(o.Launch, r)
	if r.Phase == registry.Stopped {
		stopped, err = p.stopped(local, r)
		if err != nil {
			return o, err
		}
	}
	o.State, o.Stopped = session.PlacementStopped, &stopped
	return o, nil
}
func (p *Placement) Stop(ctx context.Context, l session.Launch, i session.Incarnation) (session.Stopped, error) {
	unlock, err := p.lockOperation(l)
	if err != nil {
		return session.Stopped{}, err
	}
	defer unlock()
	local, r, err := p.retained(ctx, l)
	if err != nil {
		return session.Stopped{}, err
	}
	if inc(r) != i {
		return session.Stopped{}, &profile.Rejection{Code: profile.CodeConflict}
	}
	if r.Phase == registry.Stopped {
		fenced, err := p.physicalFence(ctx, &local, r)
		if err != nil || !fenced {
			return session.Stopped{}, errors.Join(err, errWriterFenceUnknown)
		}
		if err = p.finishStopped(ctx, local, r); err != nil {
			return session.Stopped{}, err
		}
		return p.stopped(local, r)
	}
	pod, err := p.pod(ctx, r)
	if err != nil {
		return session.Stopped{}, err
	}
	local, r, err = p.quietSource(ctx, local, r, pod)
	if err != nil {
		return session.Stopped{}, err
	}
	r, err = p.registry.Stop(ctx, r.Reference)
	if err != nil {
		return session.Stopped{}, semantic(err)
	}
	if !terminated(pod) {
		pod, err = p.waitPod(ctx, r, false)
		if err != nil {
			return session.Stopped{}, err
		}
	}
	fenced, err := p.physicalFence(ctx, &local, r)
	if err != nil || !fenced {
		return session.Stopped{}, errors.Join(err, errWriterFenceUnknown)
	}

	seals := make(map[api.CapabilityKind]budget.TrustedUsage, len(r.Ceilings))
	for name := range r.Ceilings {
		seal, err := p.ledger.Seal(ctx, account(r, name))
		if err != nil {
			fenced := session.Stopped{Launch: local.Launch, Incarnation: inc(r), ActualStopped: true}
			return fenced, fmt.Errorf("%w: %w", session.ErrUsageUnsealed, err)
		}
		seals[name] = seal
	}
	if !terminated(pod) || !running(pod, runtimekube.CaptureContainer) {
		return session.Stopped{}, errors.New("placement: exact stopped writer and capture helper unavailable")
	}
	local, err = p.capture(ctx, local, pod)
	if err != nil {
		return session.Stopped{}, err
	}
	r, err = p.registry.CompleteStop(ctx, r.Reference, local.OutputDigest, seals)
	if err != nil {
		return session.Stopped{}, semantic(err)
	}
	s, err := p.stopped(local, r)
	if err != nil {
		return s, err
	}
	if _, err = p.control(ctx, pod, runtimekube.CaptureContainer, []string{helperPath, helperFinish}, nil, 64<<10); err != nil {
		return session.Stopped{}, err
	}
	if err = p.releaseWriterFence(ctx, local, r); err != nil {
		return session.Stopped{}, err
	}
	return s, nil
}

// quietSource persists the safe-turn handoff before revoking gateway access.
// Revocation while a turn has outstanding upstream/tools could prevent that turn
// completing. A retained quiet marker permits retries without reopening grants.
// A first visit binds its genuine native UUID on the exact admitted writer
// during the handoff; the caller's empty identity can never select it.
func (p *Placement) quietSource(ctx context.Context, local retained, r registry.Record, pod *corev1.Pod) (retained, registry.Record, error) {
	if r.Phase != registry.Active && r.Phase != registry.Stopping {
		return local, r, &profile.Rejection{Code: profile.CodeConflict}
	}
	if local.NativeQuiet {
		return local, r, nil
	}
	if r.Harness == harness.NativeClaude && (terminated(pod) || r.Phase == registry.Stopping) {
		return local, r, errors.New("placement: Claude graceful completion acknowledgement unavailable")
	}
	switch {
	case terminated(pod):
		if local.Launch.ThreadID == "" {
			return local, r, errors.New("placement: initial native UUID remains unknown")
		}
	case r.Phase == registry.Stopping:
		data, err := p.native(pod)(ctx, harness.NativeStatus)
		if err != nil {
			return local, r, err
		}
		if _, err := harness.DecodeNativeReply(data, harness.NativeStatus, harness.NativeStopped, local.Launch.ThreadID); err != nil {
			return local, r, errors.New("placement: revoked source quiescence remains unknown")
		}
	default:
		bind := func(ctx context.Context, thread string) error {
			bound, err := p.registry.BindNative(ctx, r.Reference, thread)
			if err != nil {
				return semantic(err)
			}
			r = bound
			local.Launch.ThreadID = thread
			return p.save(local)
		}
		if _, err := harness.QuiesceNative(ctx, p.native(pod), local.Launch.ThreadID, local.Launch.Deadline, bind); err != nil {
			return local, r, err
		}
	}
	local.NativeQuiet = true
	return local, r, p.save(local)
}

// native runs fixed supervisor operations on the exact admitted native container.
func (p *Placement) native(pod *corev1.Pod) harness.NativeControl {
	return func(ctx context.Context, op harness.NativeOp) ([]byte, error) {
		return p.control(ctx, pod, nativeContainer, []string{nativePath, string(op)}, nil, 64<<10)
	}
}

func (p *Placement) capture(ctx context.Context, local retained, pod *corev1.Pod) (retained, error) {
	if local.OutputDigest != "" {
		return local, nil
	}
	l := local.Launch
	directory, err := harness.NativeDirectory(l.Harness)
	if err != nil {
		return local, err
	}
	args := []string{helperPath, helperCapture, "--harness", string(l.Harness), "--home", "/workspace/session/" + directory, "--workspace", "/workspace/session/workspace", "--thread", l.ThreadID, "--instruction-digest", l.InstructionDigest, "--max-bytes", strconv.FormatInt(p.config.MaxCheckpointBytes, 10)}
	for _, name := range l.IncludeIgnored {
		args = append(args, "--include-ignored", name)
	}
	data, err := p.control(ctx, pod, runtimekube.CaptureContainer, args, nil, p.config.MaxCheckpointBytes)
	if err != nil {
		return local, err
	}
	meta, err := checkpoint.Inspect(data, p.config.MaxCheckpointBytes)
	if err != nil || meta.Harness != l.Harness || meta.ThreadID != l.ThreadID {
		return local, errors.New("placement: captured checkpoint invalid")
	}
	if err = checkpoint.ValidateContinuity(data, l.Harness, l.InstructionDigest, p.config.MaxCheckpointBytes); err != nil {
		return local, err
	}
	if err = p.atomic(p.artifactPath(l.AllocationID, "output"), data); err != nil {
		return local, err
	}
	local.OutputDigest = meta.SHA256
	return local, p.save(local)
}
func (p *Placement) stopped(local retained, r registry.Record) (session.Stopped, error) {
	if !local.WriterFenced || r.Receipt == nil || local.OutputDigest != r.Receipt.CheckpointDigest {
		return session.Stopped{}, errors.New("placement: trusted checkpoint receipt unavailable")
	}
	data, err := p.readArtifact(local.Launch.AllocationID, "output")
	if err != nil {
		return session.Stopped{}, err
	}
	meta, err := checkpoint.Inspect(data, p.config.MaxCheckpointBytes)
	if err != nil || meta.SHA256 != local.OutputDigest || meta.Harness != local.Launch.Harness || meta.ThreadID != local.Launch.ThreadID {
		return session.Stopped{}, errors.New("placement: retained checkpoint invalid")
	}
	usage := make(map[string]budget.TrustedUsage, len(r.Receipt.Seals))
	for name, value := range r.Receipt.Seals {
		usage[string(name)] = value
	}
	i := inc(r)
	return session.Stopped{Launch: local.Launch, Incarnation: i, ActualStopped: true, Checkpoint: data, CheckpointDigest: local.OutputDigest, Receipt: &session.Receipt{Launch: local.Launch, Incarnation: i, Usage: usage}}, nil
}
func (p *Placement) physicalFence(ctx context.Context, local *retained, r registry.Record) (bool, error) {
	if r.RunUID == "" {
		return false, errors.New("placement: worker incarnation unavailable")
	}
	pod, err := p.exactPod(ctx, r)
	if apierrors.IsNotFound(err) {
		return local.WriterFenced, nil
	}
	if err != nil {
		return false, err
	}
	if !terminated(pod) {
		return false, nil
	}
	if !local.WriterFenced {
		local.WriterFenced = true
		if err = p.save(*local); err != nil {
			local.WriterFenced = false
			return false, err
		}
	}
	return true, nil
}

func (p *Placement) releaseWriterFence(ctx context.Context, local retained, r registry.Record) error {
	if !local.WriterFenced {
		return errWriterFenceUnknown
	}
	pod, err := p.exactPod(ctx, r)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !terminated(pod) {
		return errors.New("placement: exact writer is not terminated")
	}
	if !slices.Contains(pod.Finalizers, runtimekube.ManagedWriterFenceFinalizer) {
		return nil
	}
	original := pod.DeepCopy()
	pod.Finalizers = slices.DeleteFunc(pod.Finalizers, func(value string) bool { return value == runtimekube.ManagedWriterFenceFinalizer })
	return p.config.Kube.Patch(ctx, pod, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{}))
}

func (p *Placement) finishStopped(ctx context.Context, local retained, r registry.Record) error {
	pod, err := p.exactPod(ctx, r)
	if apierrors.IsNotFound(err) && local.WriterFenced {
		return nil
	}
	if err != nil {
		return err
	}
	if running(pod, runtimekube.CaptureContainer) {
		if _, err = p.control(ctx, pod, runtimekube.CaptureContainer, []string{helperPath, helperFinish}, nil, 64<<10); err != nil {
			return err
		}
	}
	return p.releaseWriterFence(ctx, local, r)
}
func (p *Placement) exactPod(ctx context.Context, r registry.Record) (*corev1.Pod, error) {
	var pod corev1.Pod
	if err := p.config.Kube.Get(ctx, client.ObjectKey{Namespace: p.config.AgentNamespace, Name: runtimekube.RunName(types.UID(r.RunUID))}, &pod); err != nil {
		return nil, err
	}
	if string(pod.UID) != r.PodUID || pod.Labels[runtimekube.RunUIDLabel] != r.RunUID || pod.Labels[runtimekube.RunNamespaceLabel] != p.config.Namespace {
		return nil, &profile.Rejection{Code: profile.CodeConflict}
	}
	return &pod, nil
}
func (p *Placement) Abort(ctx context.Context, l session.Launch, i session.Incarnation) (session.Observation, error) {
	unlock, err := p.lockOperation(l)
	if err != nil {
		return session.Observation{}, err
	}
	defer unlock()
	local, r, err := p.retained(ctx, l)
	if err != nil {
		return session.Observation{}, err
	}
	if inc(r) != i {
		return session.Observation{}, &profile.Rejection{Code: profile.CodeConflict}
	}
	if r.Phase == registry.Stopped {
		fenced, err := p.physicalFence(ctx, &local, r)
		if err != nil || !fenced {
			return session.Observation{}, errors.Join(err, errWriterFenceUnknown)
		}
		if err = p.finishStopped(ctx, local, r); err != nil {
			return session.Observation{}, err
		}
		stopped, err := p.stopped(local, r)
		return session.Observation{Launch: l, Incarnation: i, State: session.PlacementStopped, Stopped: &stopped}, err
	}
	r, err = p.registry.Cancel(ctx, r.Reference)
	if err != nil {
		return session.Observation{}, semantic(err)
	}
	var run api.AgentRun
	if err = p.config.Kube.Get(ctx, client.ObjectKey{Namespace: p.config.Namespace, Name: r.IntendedRunName}, &run); err == nil {
		if string(run.UID) != r.RunUID {
			return session.Observation{}, &profile.Rejection{Code: profile.CodeConflict}
		}
		original := run.DeepCopy()
		run.Spec.Cancel = true
		if err = p.config.Kube.Patch(ctx, &run, client.MergeFromWithOptions(original, client.MergeFromWithOptimisticLock{})); err != nil {
			return session.Observation{}, err
		}
	} else if !apierrors.IsNotFound(err) {
		return session.Observation{}, err
	}
	// The exact Pod is deleted with a UID precondition. A replacement at the same
	// name is never killed or accepted as proof about this incarnation.
	pod, err := p.exactPod(ctx, r)
	if err == nil {
		uid := pod.UID
		if err = p.config.Kube.Delete(ctx, pod, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			return session.Observation{}, err
		}
	} else if !apierrors.IsNotFound(err) {
		return session.Observation{}, err
	} else if !local.WriterFenced {
		return session.Observation{Launch: l, Incarnation: i, State: session.PlacementUnknown}, errors.New("placement: absent Pod has no retained writer fence")
	}
	o := session.Observation{Launch: l, Incarnation: i, State: session.PlacementUnknown}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		fenced, err := p.physicalFence(ctx, &local, r)
		if err != nil {
			return o, err
		}
		if fenced {
			o.State = session.PlacementStopped
			if r.Receipt == nil {
				// Never initialize an absent account during cancellation. Failed or
				// incomplete seals leave usage unknown despite a known writer fence.
				if sealed, err := p.sealCancellation(ctx, r); err == nil {
					r = sealed
				}
			}
			if err = p.releaseWriterFence(ctx, local, r); err != nil {
				return o, err
			}
			stopped := cancelledStopped(l, r)
			o.Stopped = &stopped
			return o, nil
		}
		select {
		case <-ctx.Done():
			return o, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (p *Placement) sealCancellation(ctx context.Context, r registry.Record) (registry.Record, error) {
	seals := make(map[api.CapabilityKind]budget.TrustedUsage, len(r.Ceilings))
	for name := range r.Ceilings {
		seal, err := p.ledger.Seal(ctx, account(r, name))
		if err != nil {
			return r, err
		}
		seals[name] = seal
	}
	sealed, err := p.registry.CompleteCancel(ctx, r.Reference, seals)
	return sealed, semantic(err)
}

// Called only after independently observing the exact writer fence. A nil
// registry receipt is explicit unknown usage; it is never synthesized as zero.
func cancelledStopped(l session.Launch, r registry.Record) session.Stopped {
	i := inc(r)
	s := session.Stopped{Launch: l, Incarnation: i, ActualStopped: true}
	if r.Receipt != nil {
		usage := make(map[string]budget.TrustedUsage, len(r.Receipt.Seals))
		for name, value := range r.Receipt.Seals {
			usage[string(name)] = value
		}
		s.Receipt = &session.Receipt{Launch: l, Incarnation: i, Usage: usage}
	}
	return s
}

func (p *Placement) Attach(ctx context.Context, l session.Launch, i session.Incarnation, input io.Reader, output io.Writer) error {
	unlock, err := p.lockOperation(l)
	if err != nil {
		return err
	}
	_, r, err := p.retained(ctx, l)
	unlock()
	if err != nil {
		return err
	}
	if inc(r) != i || r.Phase != registry.Active {
		return &profile.Rejection{Code: profile.CodeConflict}
	}
	if _, err = p.liveProfile(ctx, l); err != nil {
		return err
	}
	pod, err := p.activePod(ctx, r)
	if err != nil {
		return err
	}
	if !running(pod, nativeContainer) || !pod.DeletionTimestamp.IsZero() {
		return &profile.Rejection{Code: profile.CodeConflict}
	}
	ctx, cancel := context.WithDeadline(ctx, l.Deadline)
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				current, err := p.registry.Get(ctx, r.Reference)
				if err != nil || current.Phase != registry.Active || inc(current) != i {
					cancel()
					return
				}
				if _, err = p.liveProfile(ctx, l); err != nil {
					cancel()
					return
				}
				currentPod, err := p.activePod(ctx, current)
				if err != nil || !running(currentPod, nativeContainer) || !currentPod.DeletionTimestamp.IsZero() {
					cancel()
					return
				}
			}
		}
	}()
	return p.exec.Execute(ctx, pod.Namespace, pod.Name, nativeContainer, []string{nativePath, string(harness.NativeAttach)}, input, output, &boundedOutput{max: 16 << 10})
}
