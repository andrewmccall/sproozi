package placement

import (
	"bytes"
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
	"github.com/andrewmccall/sproozi/internal/deadline"
	"github.com/andrewmccall/sproozi/internal/harness"
	runtimekube "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/sandboxpod"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var errWriterFenceUnknown = errors.New("placement: writer fence remains unknown")

// errRunUnavailable is a read of a visit's Pod whose run is not the bound one
// or has no sandbox: queued, or ended before its sandbox started.
var errRunUnavailable = errors.New("placement: exact admitted run unavailable")

// podStopWait bounds a stop's wait for the native container to end after the
// supervisor's graceful handoff, within the stop's settle allowance
// (profile.StopBound). A container that has not ended by then leaves the stop
// unproven, and its retry waits again. Tests shorten it.
var podStopWait = time.Minute

// retained reads l's retained record and its session registration for an
// operation that starts or extends work. A record that contradicts itself or
// its registration is refused: nothing gains authority from it.
func (p *Placement) retained(ctx context.Context, l session.Launch) (retained, registry.Record, error) {
	return p.retainedRecord(ctx, l, true)
}

// observing reads them for an operation that only observes the visit:
// Inspect and Exited. A contradiction must not keep a visit from being
// listed, stopped, cancelled and removed, so these continue from the facts
// that hold, observing a forgotten fact again or reporting it unknown. They
// do not save the record to correct it, so it keeps its contradiction until
// an operation that ends the visit reads it; a save Inspect makes for its
// own reason, such as a fence it observes, writes the corrected record.
func (p *Placement) observing(ctx context.Context, l session.Launch) (retained, registry.Record, error) {
	local, r, err := p.retainedRecord(ctx, l, false)
	return local, r, forgiven(err)
}

// settling reads them for an operation that ends the visit: Stop and
// Abort. It continues as observing does, and first saves the record as
// corrected (ADR 0066).
func (p *Placement) settling(ctx context.Context, l session.Launch) (retained, registry.Record, error) {
	local, r, err := p.retainedRecord(ctx, l, false)
	var contradicted *contradiction
	if errors.As(err, &contradicted) {
		return local, r, p.save(local, &r)
	}
	return local, r, err
}

// forgiven is err unless it is only a contradiction, which an operation
// that ends or observes the visit continues past.
func forgiven(err error) error {
	var contradicted *contradiction
	if errors.As(err, &contradicted) {
		return nil
	}
	return err
}

// noted logs a contradiction in err once per contradiction and process, and
// returns err. The sweep for exited harnesses reads an active visit every
// few seconds; logging each read would repeat the line until it stops.
func (p *Placement) noted(err error) error {
	var contradicted *contradiction
	if !errors.As(err, &contradicted) {
		return err
	}
	if _, seen := p.contradictions.LoadOrStore(contradicted.Error(), struct{}{}); !seen {
		p.logf("Found that %v; operations that start work refuse it until the visit is stopped or cancelled", contradicted)
	}
	return err
}

// retainedRecord reads l's retained record and registration. With refuse, a
// contradiction is returned before anything changes; without, the read
// continues from the facts that hold and returns the contradiction last.
func (p *Placement) retainedRecord(ctx context.Context, l session.Launch, refuse bool) (retained, registry.Record, error) {
	local, loaded := p.load(l)
	var contradicted *contradiction
	if loaded != nil && (refuse || !errors.As(loaded, &contradicted)) {
		return local, registry.Record{}, loaded
	}
	local, r, err := p.registered(ctx, l, local)
	if err != nil && (refuse || !errors.As(err, &contradicted)) {
		return local, r, err
	}
	created := err
	local, err = local.check(&r)
	if err = p.noted(err); err != nil && refuse {
		return local, r, err
	}
	return local, r, errors.Join(loaded, created, err)
}

// adopt records the registration r in local, the record of r's launch,
// takes from r what its phase proves and saves the result, before anything
// else reads it. Create replays a registration that already exists, which
// may have moved on while this record was lost: saving the bare reference
// beside a stopped registration would be refused, and every later read with
// it. It returns the contradiction it corrected, if any, once saved.
func (p *Placement) adopt(local retained, r registry.Record) (retained, error) {
	local.Reference = r.Reference
	local, checked := local.check(&r)
	if err := p.save(local, &r); err != nil {
		return local, err
	}
	return local, p.noted(checked)
}

// registered reads, or creates, the registration local names, and checks
// that it is the registration of l, local's launch.
func (p *Placement) registered(ctx context.Context, l session.Launch, local retained) (retained, registry.Record, error) {
	if local.PreparationAborted {
		// An aborted preparation never creates its registration, and nothing
		// activates, stops or attaches to it: only AbortPreparation, which
		// does not come through here, may finish it.
		return local, registry.Record{}, &profile.Rejection{Code: profile.CodeConflict}
	}
	if local.Reference.UID == "" {
		ceilings, err := p.liveProfileInput(ctx, l, local.fresh())
		if err != nil {
			return local, registry.Record{}, err
		}
		registration := p.registration(l, ceilings)
		registration.WorkspaceDigest = local.WorkspaceDigest
		r, err := p.registry.Create(ctx, registration)
		if err != nil {
			return local, r, semantic(err)
		}
		local, err = p.adopt(local, r)
		return local, r, err
	}
	r, err := p.registry.Get(ctx, local.Reference)
	if err != nil {
		return local, r, semantic(err)
	}
	l = local.Launch
	// Only a first visit's registration gains a native UUID after its launch.
	thread := r.NativeUUID == l.ThreadID || local.fresh() && l.ThreadID == ""
	if r.Harness != p.config.Harness || l.Harness != r.Harness || r.Principal != p.config.Principal || r.TemplateName != p.config.TemplateName || r.Epoch != l.Epoch || r.IntendedRunName != runName(l) || r.OperationID != l.OperationID || r.SessionID != l.ExecutionID || r.WorkspaceDigest != local.WorkspaceDigest || r.Task != l.Task || !thread || r.CheckpointDigest != l.CheckpointDigest || r.InstructionDigest != l.InstructionDigest || !r.OriginalDeadline.Equal(l.Deadline) || !slices.Equal(r.IncludeIgnored, l.IncludeIgnored) || r.ProfileDigest != l.ProfileDigest || !ceilingsMatch(l, r.Ceilings) {
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
		return nil, errRunUnavailable
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
func (p *Placement) waitPod(ctx context.Context, r registry.Record, wantRunning bool) (*corev1.Pod, error) {
	// An interactive visit must be running within two minutes; otherwise the
	// wait lasts until the execution deadline, or the caller, when there is none.
	until := deadline.At(r.OriginalDeadline)
	if wantRunning && r.Task == "" {
		until = deadline.At(until.Bound(time.Now(), 2*time.Minute))
	}
	if !wantRunning {
		// A stop waits for a container whose supervisor has already ended
		// its harness, so it ends within seconds, deadline or not.
		until = deadline.At(until.Bound(time.Now(), podStopWait))
	}
	ctx, cancel := until.Context(ctx)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		pod, err := p.pod(ctx, r)
		if err == nil {
			seen := sandboxpod.Observe(pod)
			if wantRunning && seen.Agent.Running && seen.Capture.Running && !seen.Deleting {
				return pod, nil
			}
			if !wantRunning && seen.Agent.Ended() {
				return pod, nil
			}
		}
		if err != nil && !apierrors.IsNotFound(err) && r.PodUID != "" {
			return nil, err
		}
		if wantRunning {
			// A start whose run can no longer get a Pod would otherwise wait
			// for the deadline, or for ever without one (ADR 0067).
			if refused := p.startRefused(ctx, r); refused != nil {
				return nil, refused
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// startRefused is the refusal of a start whose bound run can no longer get
// a running Pod, or nil while it still can. A run can no longer get one once
// it is terminal, cancelled, deleted or replaced, or once its registration
// has left Creating and Bound, which another operation's cancellation does:
// the controller creates a Pod only for a live run that a bound registration
// names. A run that is queued under a cap, admitted while a ResourceQuota
// refuses its Pod, or that named a Pod not yet created, can still get one,
// so it is not refused. A read that fails is no evidence either way: the
// wait reads again.
func (p *Placement) startRefused(ctx context.Context, r registry.Record) error {
	refuse := func(format string, args ...any) error {
		return &session.StartRefused{Reason: session.RefusalReason(fmt.Sprintf(format, args...))}
	}
	var run api.AgentRun
	err := p.config.Kube.Get(ctx, client.ObjectKey{Namespace: p.config.Namespace, Name: r.IntendedRunName}, &run)
	switch {
	case apierrors.IsNotFound(err):
		return refuse("AgentRun %s was deleted before its sandbox started", r.IntendedRunName)
	case err != nil:
		return nil
	case string(run.UID) != r.RunUID:
		return refuse("AgentRun %s was replaced before its sandbox started: it now has UID %s, not %s",
			r.IntendedRunName, run.UID, r.RunUID)
	case run.Status.Phase.IsTerminal():
		return refuse("%s", runEnded(&run))
	case run.Spec.Cancel:
		return refuse("AgentRun %s was cancelled before its sandbox started", r.IntendedRunName)
	}
	current, err := p.registry.Get(ctx, r.Reference)
	switch {
	case errors.Is(err, registry.ErrGone):
		return refuse("the session registration for AgentRun %s was deleted before its sandbox started", r.IntendedRunName)
	case err != nil:
		return nil
	case current.Phase != registry.Creating && current.Phase != registry.Bound:
		return refuse("the session registration for AgentRun %s is %s, so its sandbox can never start",
			r.IntendedRunName, current.Phase)
	}
	return nil
}

// runEnded says how a terminal run ended before its sandbox started, with
// the reason and message of its Ready condition when it has one.
func runEnded(run *api.AgentRun) string {
	how := map[api.AgentRunPhase]string{api.AgentRunPhaseFailed: "failed", api.AgentRunPhaseTimedOut: "timed out",
		api.AgentRunPhaseCancelled: "was cancelled", api.AgentRunPhaseSucceeded: "succeeded"}[run.Status.Phase]
	s := fmt.Sprintf("AgentRun %s %s before its sandbox started", run.Name, how)
	if ready := apimeta.FindStatusCondition(run.Status.Conditions, readyCondition); ready != nil && ready.Reason != "" {
		s += ": " + ready.Reason
		if ready.Message != "" {
			s += ": " + ready.Message
		}
	}
	return s
}

// readyCondition is the AgentRun condition that carries the controller's
// reason for the run's phase.
const readyCondition = "Ready"

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
	if _, err = p.liveProfileInput(ctx, local.Launch, local.fresh()); err != nil {
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
	if seen := sandboxpod.Observe(pod); !seen.Agent.Running || seen.Deleting {
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
	if local.fresh() {
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
	local, r, err := p.observing(ctx, l)
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
		if apierrors.IsNotFound(err) || r.PodUID == "" && errors.Is(err, errRunUnavailable) {
			// No Pod is bound yet: recovery calls Prepare again, which waits
			// for one or refuses a run that can never get it (ADR 0067).
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
		if !sandboxpod.Observe(pod).Agent.Running {
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
		if seen := sandboxpod.Observe(pod); !seen.Agent.Running || seen.Deleting {
			return o, nil
		}
		data, err := p.native(pod)(ctx, harness.NativeStatus)
		if err != nil {
			return o, err
		}
		_, activeErr := harness.DecodeNativeReply(data, harness.NativeStatus, harness.NativeActive, l.ThreadID)
		if local.fresh() {
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
	stopped := cancelledStopped(local, r)
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
	local, r, err := p.settling(ctx, l)
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
	if !sandboxpod.Observe(pod).Agent.Ended() {
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
			fenced := session.Stopped{Launch: local.Launch, Incarnation: inc(r), ActualStopped: true, ThreadID: local.thread(r)}
			return fenced, fmt.Errorf("%w: %w", session.ErrUsageUnsealed, err)
		}
		seals[name] = seal
	}
	if seen := sandboxpod.Observe(pod); !seen.Agent.Ended() || !seen.Capture.Running {
		return session.Stopped{}, errors.New("placement: exact stopped writer and capture helper unavailable")
	}
	if local, err = p.keep(ctx, local, r, pod); err != nil {
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
	if !harness.ProvesCompletion(r.Harness) && r.Phase == registry.Stopping && !sandboxpod.Observe(pod).Agent.Ended() {
		return local, r, fmt.Errorf("placement: %s graceful completion acknowledgement unavailable", r.Harness)
	}
	switch {
	case sandboxpod.Observe(pod).Agent.Ended():
		var err error
		if local, r, err = p.ended(ctx, local, r, pod); err != nil {
			return local, r, err
		}
	case r.Phase == registry.Stopping:
		data, err := p.native(pod)(ctx, harness.NativeStatus)
		if err != nil {
			return local, r, err
		}
		reply, err := harness.DecodeNativeReply(data, harness.NativeStatus, harness.NativeStopped, local.thread(r))
		if err != nil || local.thread(r) == "" && !reply.Turnless {
			return local, r, errors.New("placement: revoked source quiescence remains unknown")
		}
		local.Turnless = local.thread(r) == ""
	default:
		bind := func(ctx context.Context, thread string) error {
			bound, err := p.registry.BindNative(ctx, r.Reference, thread)
			if err != nil {
				return semantic(err)
			}
			r = bound
			return nil
		}
		thread, err := harness.QuiesceNative(ctx, p.native(pod), local.thread(r), local.Launch.Deadline, bind)
		switch {
		case err == nil:
			// Only a turnless first visit quiesces without a bound thread;
			// it has no conversation to capture (ADR 0032).
			local.Turnless = thread == ""
		case errors.Is(err, harness.ErrQuiesceUnacknowledged) && harness.ProvesCompletion(r.Harness) && ctx.Err() == nil:
			// The reply is usually lost because the container ended first,
			// as on Docker: the container's end decides.
			ended, ok := p.endsWithin(ctx, r, lostReplyWait)
			if !ok {
				return local, r, err
			}
			if local, r, err = p.ended(ctx, local, r, ended); err != nil {
				return local, r, err
			}
		default:
			return local, r, err
		}
	}
	local.NativeQuiet = true
	return local, r, p.save(local, &r)
}

// ended settles a stop of a visit whose native container already ended, once
// a stop was requested. A harness whose state proves completion, whose
// supervisor exited 0 and whose output, read in full, holds no exit report
// was stopped by a graceful quiesce: this placement asked for it and lost the
// reply, as the Docker placement treats a lost reply. The supervisor writes a
// report for every other ending (ADR 0038). Anything else is an exit
// (recordExit). A Claude Code container never proves its turn, so a lost
// reply there is an exit, which keeps what the visit started from, as on
// Docker (ADR 0011).
//
// A first visit that ended before any completed turn has no conversation to
// capture (ADR 0032). A stopped container never proves a completed turn of a
// harness such as Claude Code, so its first visit binds none and is turnless
// too. One that bound a conversation before it exited keeps nothing of it
// (keep).
func (p *Placement) ended(ctx context.Context, local retained, r registry.Record, pod *corev1.Pod) (retained, registry.Record, error) {
	if local.Exited == nil {
		var code int32 // the caller saw the native container end
		if ended := sandboxpod.Observe(pod).Agent.Terminated; ended != nil {
			code = ended.ExitCode
		}
		logs, logErr := p.exec.Logs(ctx, pod.Namespace, pod.Name, nativeContainer)
		graceful := harness.ProvesCompletion(r.Harness) && code == 0 && logErr == nil &&
			!bytes.Contains(logs, []byte(harness.NativeExitReportPrefix))
		if !graceful {
			var err error
			if local, r, err = p.recordExit(ctx, local, r, code, logs, logErr); err != nil {
				return local, r, err
			}
		}
	}
	local.Turnless = local.thread(r) == ""
	return local, r, nil
}

// lostReplyWait bounds a stop's wait, after its quiesce reply was lost, for
// the native container to be seen to end, as the Docker placement waits for
// its agent container. Tests shorten it.
var lostReplyWait = 10 * time.Second

// endsWithin reports the visit's Pod once its native container is seen to
// end within wait.
func (p *Placement) endsWithin(ctx context.Context, r registry.Record, wait time.Duration) (*corev1.Pod, bool) {
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if pod, err := p.pod(ctx, r); err == nil && sandboxpod.Observe(pod).Agent.Ended() {
			return pod, true
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-ticker.C:
		}
	}
}

// recordExit keeps what the supervisor reported when its harness ended
// without a graceful stop, as the Docker placement does: the report in the
// native container's last output lines, or the container's exit code when
// there is none, with the deadline then decided by this host's clock. A
// first visit of a harness whose state proves completion binds the thread of
// a completed turn it reported, so the stop can still checkpoint that
// conversation.
func (p *Placement) recordExit(ctx context.Context, local retained, r registry.Record, code int32, logs []byte, logErr error) (retained, registry.Record, error) {
	report := harness.NativeExitReport{Code: int(code)}
	if logErr == nil {
		if reported, ok := harness.ParseNativeExitReport(logs); ok {
			report = reported
		}
	}
	exit := &session.Exit{Code: report.Code, Output: report.Output, Cause: report.Cause}
	switch report.Cause {
	case harness.NativeExitDeadline:
		exit.Deadline = true
	case harness.NativeExitSelf, harness.NativeExitFailed:
	default:
		exit.Deadline = deadline.At(local.Launch.Deadline).Passed(time.Now())
	}
	if harness.ProvesCompletion(r.Harness) && local.thread(r) == "" && report.Thread != "" {
		bound, err := p.registry.BindNative(ctx, r.Reference, report.Thread)
		if err != nil {
			return local, r, semantic(err)
		}
		r = bound
	}
	local.Exited = exit
	return local, r, p.save(local, &r)
}

// Exited reports whether an active visit's native container ended without a
// stop being requested: its harness exited on its own or at its deadline.
// Stopping the visit then reports the exit. A visit another operation holds,
// such as a stop in progress, is being acted on: Exited reports no exit
// rather than wait for it.
func (p *Placement) Exited(ctx context.Context, l session.Launch, i session.Incarnation) (bool, error) {
	unlock, busy, err := p.tryLockOperation(l)
	if err != nil || busy {
		return false, err
	}
	defer unlock()
	local, r, err := p.observing(ctx, l)
	if err != nil {
		return false, err
	}
	if inc(r) != i {
		return false, &profile.Rejection{Code: profile.CodeConflict}
	}
	if r.Phase != registry.Active || local.NativeQuiet {
		return false, nil
	}
	pod, err := p.pod(ctx, r)
	if err != nil {
		return false, err
	}
	return sandboxpod.Observe(pod).Agent.Ended(), nil
}

// native runs fixed supervisor operations on the exact admitted native container.
func (p *Placement) native(pod *corev1.Pod) harness.NativeControl {
	return func(ctx context.Context, op harness.NativeOp) ([]byte, error) {
		return p.control(ctx, pod, nativeContainer, []string{nativePath, string(op)}, nil, 64<<10)
	}
}

func (p *Placement) capture(ctx context.Context, local retained, r registry.Record, pod *corev1.Pod) (retained, error) {
	if local.OutputDigest != "" {
		return local, nil
	}
	l, thread := local.Launch, local.thread(r)
	directory, err := harness.NativeDirectory(l.Harness)
	if err != nil {
		return local, err
	}
	args := []string{helperPath, helperCapture, "--harness", string(l.Harness), "--home", "/workspace/session/" + directory, "--workspace", "/workspace/session/workspace", "--thread", thread, "--instruction-digest", l.InstructionDigest, "--max-bytes", strconv.FormatInt(p.config.MaxCheckpointBytes, 10)}
	for _, name := range l.IncludeIgnored {
		args = append(args, "--include-ignored", name)
	}
	data, err := p.control(ctx, pod, runtimekube.CaptureContainer, args, nil, p.config.MaxCheckpointBytes)
	if err != nil {
		return local, captureFailure(err)
	}
	// What came back is the fenced state's checkpoint: a failure of these
	// checks is the state's, and capturing again finds the same.
	meta, err := checkpoint.Inspect(data, p.config.MaxCheckpointBytes)
	if err != nil || meta.Harness != l.Harness || meta.ThreadID != thread {
		return local, fmt.Errorf("%w: placement: captured checkpoint invalid", session.ErrUncapturable)
	}
	if err = checkpoint.ValidateContinuity(data, l.Harness, l.InstructionDigest, p.config.MaxCheckpointBytes); err != nil {
		return local, fmt.Errorf("%w: %w", session.ErrUncapturable, err)
	}
	if err = p.atomic(p.artifactPath(l.AllocationID, "output"), data); err != nil {
		return local, err
	}
	local.OutputDigest = meta.SHA256
	return local, p.save(local, &r)
}

// captureFailure maps a failed capture-stopped command onto the shared rule
// (session.FallsBack, ADR 0062). The helper's checkpoint.ExitStateRefused is
// a refusal of the fenced state, session.ErrUncapturable. Its
// ExitCaptureFailed, an exec or stream failure, a killed helper and any other
// code fail the stop, and its retry captures again. Code 1 is what a helper
// image older than the codes returns for every failure, which cannot be told
// apart: this placement fails the stop for it, as it did before the codes,
// and a newer image says which it is. The legacy mapping goes once images
// older than the codes are no longer supported.
func captureFailure(err error) error {
	var failed *commandFailed
	if errors.As(err, &failed) && failed.exited && failed.code == checkpoint.ExitStateRefused {
		return fmt.Errorf("%w: the capture helper refused the stopped state", session.ErrUncapturable)
	}
	return err
}

// keep retains what the fenced visit keeps, by the rule every placement
// shares (session.StopKeeps, ADR 0062). A turnless first visit, or a first
// visit whose harness exited without proving a completed turn, keeps no
// checkpoint: the session keeps the workspace it started from. A resumed
// visit that proved nothing keeps the checkpoint it started from. An exited
// harness whose captured state does not validate keeps the same instead of
// failing the stop on every retry (session.FallsBack), and the discard is
// logged; every other capture failure fails the stop, and its retry captures
// again. A resumed visit's fallback is retained before the receipt is
// recorded: only a failure of the fenced state reaches it, which a retry
// would meet again.
func (p *Placement) keep(ctx context.Context, local retained, r registry.Record, pod *corev1.Pod) (retained, error) {
	keep, onFailure := session.StopKeeps(session.VisitEnd{First: local.fresh(), Turnless: local.Turnless,
		Exited: local.Exited != nil, ProvesCompletion: harness.ProvesCompletion(r.Harness), Conversation: local.thread(r) != ""})
	if keep == session.KeptCapture {
		captured, err := p.capture(ctx, local, r, pod)
		if err == nil {
			return captured, nil
		}
		if !session.FallsBack(ctx, err, onFailure) {
			return local, err
		}
		p.logf("Discarded the captured state of stopped visit %s of execution %s and kept the %s it started from: %v",
			local.Launch.AllocationID, local.Launch.ExecutionID, map[session.Kept]string{session.KeptInput: "checkpoint",
				session.KeptWorkspace: "workspace"}[onFailure], err)
		keep = onFailure
	}
	if keep == session.KeptInput {
		return p.keepInput(local, r)
	}
	return local, nil
}

// keepInput makes the checkpoint a resumed visit started from its output:
// the visit's harness ended without proving a completed turn, so nothing it
// did is kept.
func (p *Placement) keepInput(local retained, r registry.Record) (retained, error) {
	if local.OutputDigest != "" {
		return local, nil
	}
	l := local.Launch
	data, err := p.readArtifact(l.AllocationID, "input")
	if err != nil {
		return local, err
	}
	meta, err := checkpoint.Inspect(data, p.config.MaxCheckpointBytes)
	if err != nil || meta.SHA256 != l.CheckpointDigest || meta.Harness != l.Harness || meta.ThreadID != l.ThreadID {
		return local, errors.New("placement: retained input checkpoint invalid")
	}
	if err = p.atomic(p.artifactPath(l.AllocationID, "output"), data); err != nil {
		return local, err
	}
	local.OutputDigest = meta.SHA256
	return local, p.save(local, &r)
}

func (p *Placement) stopped(local retained, r registry.Record) (session.Stopped, error) {
	if !local.WriterFenced || r.Receipt == nil || local.OutputDigest != r.Receipt.CheckpointDigest {
		return session.Stopped{}, errors.New("placement: trusted checkpoint receipt unavailable")
	}
	usage := make(map[string]budget.TrustedUsage, len(r.Receipt.Seals))
	for name, value := range r.Receipt.Seals {
		usage[string(name)] = value
	}
	i := inc(r)
	s := session.Stopped{Launch: local.Launch, Incarnation: i, ActualStopped: true, ThreadID: local.thread(r), Receipt: &session.Receipt{Launch: local.Launch, Incarnation: i, Usage: usage}, Exit: local.Exited}
	// The registry keeps a receipt with no checkpoint only for a first visit:
	// a turnless one, with no thread, or one whose harness exited without
	// proving its conversation, which reports that thread with its exit, as
	// the Docker placement does. Neither has anything to return.
	if r.Receipt.CheckpointDigest == "" {
		if s.ThreadID != "" && local.Exited == nil {
			return session.Stopped{}, errors.New("placement: stop without a checkpoint names a native thread")
		}
		return s, nil
	}
	data, err := p.readArtifact(local.Launch.AllocationID, "output")
	if err != nil {
		return session.Stopped{}, err
	}
	meta, err := checkpoint.Inspect(data, p.config.MaxCheckpointBytes)
	if err != nil || meta.SHA256 != local.OutputDigest || meta.Harness != local.Launch.Harness || meta.ThreadID != local.thread(r) {
		return session.Stopped{}, errors.New("placement: retained checkpoint invalid")
	}
	s.Checkpoint, s.CheckpointDigest = data, local.OutputDigest
	return s, nil
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
	if !sandboxpod.Observe(pod).Agent.Ended() {
		return false, nil
	}
	if !local.WriterFenced {
		local.WriterFenced = true
		if err = p.save(*local, &r); err != nil {
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
	if seen := sandboxpod.Observe(pod); !seen.Agent.Ended() && !seen.NeverScheduled {
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
	if sandboxpod.Observe(pod).Capture.Running {
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
	local, r, err := p.settling(ctx, l)
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
	o := session.Observation{Launch: l, Incarnation: i, State: session.PlacementUnknown}
	if r, err = p.fenceCancelled(ctx, &local, r); err != nil {
		return o, err
	}
	stopped := cancelledStopped(local, r)
	o.State, o.Stopped = session.PlacementStopped, &stopped
	return o, nil
}

// fenceCancelled forcibly stops the exact Pod bound to a cancelled
// registration and returns once its writer is fenced: its native container
// was observed terminated, or the Pod was never scheduled. It then seals the
// usage when it can and releases the writer-fence finalizer. A Pod gone
// without retained fence evidence stays unknown (ADR 0010).
func (p *Placement) fenceCancelled(ctx context.Context, local *retained, r registry.Record) (registry.Record, error) {
	// The exact Pod is deleted with a UID precondition. A replacement at the same
	// name is never killed or accepted as proof about this incarnation.
	pod, err := p.exactPod(ctx, r)
	if err == nil {
		uid := pod.UID
		if err = p.config.Kube.Delete(ctx, pod, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil && !apierrors.IsNotFound(err) {
			return r, err
		}
	} else if !apierrors.IsNotFound(err) {
		return r, err
	} else if !local.WriterFenced {
		return r, errors.New("placement: absent Pod has no retained writer fence")
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		fenced, err := p.physicalFence(ctx, local, r)
		if err == nil && !fenced {
			fenced, err = p.unscheduledFence(ctx, local, r)
		}
		if err != nil {
			return r, err
		}
		if fenced {
			if r.Receipt == nil {
				// Never initialize an absent account during cancellation. Failed or
				// incomplete seals leave usage unknown despite a known writer fence.
				if sealed, err := p.sealCancellation(ctx, r); err == nil {
					r = sealed
				}
			}
			return r, p.releaseWriterFence(ctx, *local, r)
		}
		select {
		case <-ctx.Done():
			return r, fmt.Errorf("%w: Pod %s/%s of AgentRun %s was deleted but its agent container has not been seen to stop: %w",
				errWriterFenceUnknown, p.config.AgentNamespace, runtimekube.RunName(types.UID(r.RunUID)), r.IntendedRunName, ctx.Err())
		case <-ticker.C:
		}
	}
}

// unscheduledFence records the fence of an exact Pod that is being deleted
// and never ran on any node.
func (p *Placement) unscheduledFence(ctx context.Context, local *retained, r registry.Record) (bool, error) {
	pod, err := p.exactPod(ctx, r)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil || !sandboxpod.Observe(pod).NeverScheduled {
		return false, err
	}
	if !local.WriterFenced {
		local.WriterFenced = true
		if err = p.save(*local, &r); err != nil {
			local.WriterFenced = false
			return false, err
		}
	}
	return true, nil
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
func cancelledStopped(local retained, r registry.Record) session.Stopped {
	l, i := local.Launch, inc(r)
	s := session.Stopped{Launch: l, Incarnation: i, ActualStopped: true, ThreadID: local.thread(r)}
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
	local, r, err := p.retained(ctx, l)
	unlock()
	if err != nil {
		return err
	}
	if inc(r) != i || r.Phase != registry.Active {
		return &profile.Rejection{Code: profile.CodeConflict}
	}
	// Check the retained launch the way stop and inspect do: a first visit
	// from a workspace has no conversation ID or checkpoint yet.
	live := func(ctx context.Context) error {
		_, err := p.liveProfileInput(ctx, local.Launch, local.fresh())
		return err
	}
	if err = live(ctx); err != nil {
		return err
	}
	pod, err := p.activePod(ctx, r)
	if err != nil {
		return err
	}
	if seen := sandboxpod.Observe(pod); !seen.Agent.Running || seen.Deleting {
		return &profile.Rejection{Code: profile.CodeConflict}
	}
	ctx, cancel := deadline.At(l.Deadline).Context(ctx)
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
				if err = live(ctx); err != nil {
					cancel()
					return
				}
				currentPod, err := p.activePod(ctx, current)
				if err != nil || !sandboxpod.Observe(currentPod).Agent.Running || sandboxpod.Observe(currentPod).Deleting {
					cancel()
					return
				}
			}
		}
	}()
	return p.exec.Execute(ctx, pod.Namespace, pod.Name, nativeContainer, []string{nativePath, string(harness.NativeAttach)}, input, output, &boundedOutput{max: 16 << 10, truncate: true})
}
