package docker

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/workspace"
	"github.com/google/uuid"
)

const (
	phaseCreating            = "creating"
	phasePrepared            = "prepared"
	phaseActivating          = "activating"
	phaseActive              = "active"
	phaseStopping            = "stopping"
	phaseStopped             = "stopped"
	phaseAborting            = "aborting"
	phaseAborted             = "aborted"
	phasePreparationAborting = "preparation-aborting"
	roleAgent                = "agent"
	roleHelper               = "helper"
	roleAnchor               = "anchor"
	nativeCommandPath        = "/usr/local/bin/native-session"
	checkpointCommandPath    = "/usr/local/bin/checkpoint"
	networkCommandPath       = "/usr/local/bin/sproozi-network"
	launchLabel              = "sproozi.com/launch"
	nativeUser               = "1000:1000"
)

var _ session.Placement = (*Placement)(nil)
var _ session.TerminalPlacement = (*Placement)(nil)
var _ session.PreparationAborter = (*Placement)(nil)

type NativeAccess struct {
	TrustBundlePEM []byte
	Env            []string
	Model          string
	Options        []string
	ClientConfig   *harness.NativeClientConfig
	// UserInstructions are shared user preferences, never approved instructions.
	UserInstructions string
}

// Authority is the trusted home gateway/accounting owner. It persists exact
// allocation/container bindings; native diagnostics and checkpoint bytes never
// initialize, activate, revoke or settle their own authority.
type Authority interface {
	InitializeAllocation(context.Context, session.Launch, session.Incarnation) (NativeAccess, error)
	Activate(context.Context, session.Launch, session.Incarnation) error
	Revoke(context.Context, session.Launch, session.Incarnation) error
	Seal(context.Context, session.Launch, session.Incarnation) (map[string]budget.TrustedUsage, error)
	BindNative(context.Context, session.Launch, session.Incarnation, string) error
}
type ResourceLimits struct{ MemoryBytes, CPUMilli, PIDs int64 }
type PlacementConfig struct {
	Harness                                        harness.NativeKind
	Name                                           string
	NativeImageID, HelperImageID, AnchorImageID    string
	NetworkName, GatewayIPv4                       string
	GatewayPort                                    uint16
	Instructions                                   string
	InstructionDigest                              string
	AgentLimits, HelperLimits, AnchorLimits        ResourceLimits
	VolumeBytes, MaxCheckpointBytes, RetainedBytes int64
}

func (c PlacementConfig) validate() error {
	if c.Harness.Validate() != nil {
		return errors.New("docker placement requires an explicit supported native harness")
	}
	if c.NetworkName == "host" || c.NetworkName == "none" {
		return errors.New("docker anchor requires a bridge network")
	}
	ip, err := netip.ParseAddr(c.GatewayIPv4)
	if !resourceName.MatchString(c.Name) || !resourceName.MatchString(c.NetworkName) || err != nil || !ip.Is4() || ip.IsUnspecified() || ip.IsMulticast() || c.GatewayPort == 0 || c.Instructions == "" || fmt.Sprintf("%x", sha256.Sum256([]byte(c.Instructions))) != c.InstructionDigest || c.VolumeBytes < 16<<20 || c.VolumeBytes > 4<<30 || c.MaxCheckpointBytes <= 0 || c.MaxCheckpointBytes > 64<<20 || c.RetainedBytes < c.MaxCheckpointBytes*2 {
		return errors.New("invalid Docker placement configuration")
	}
	for _, image := range []string{c.NativeImageID, c.HelperImageID, c.AnchorImageID} {
		if !strings.HasPrefix(image, "sha256:") || !objectID.MatchString(strings.TrimPrefix(image, "sha256:")) {
			return errors.New("docker placement requires pinned images")
		}
	}
	for _, limit := range []ResourceLimits{c.AgentLimits, c.HelperLimits, c.AnchorLimits} {
		if limit.MemoryBytes < 16<<20 || limit.MemoryBytes > 16<<30 || limit.CPUMilli < 1 || limit.CPUMilli > 16000 || limit.PIDs < 1 || limit.PIDs > 1024 {
			return errors.New("invalid Docker placement resource limits")
		}
	}
	return nil
}
func validUUID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s && u != uuid.Nil
}

// validateLaunch checks a launch against this placement. A first visit from a
// Git workspace names neither a native thread nor a checkpoint.
func (p *Placement) validateLaunch(l session.Launch) error {
	initial := l.CheckpointDigest == ""
	if err := workspace.ValidateIgnoredPaths(l.IncludeIgnored); err != nil {
		return err
	}
	if l.Harness != p.config.Harness || !validUUID(l.ExecutionID) || !validUUID(l.OperationID) || !validUUID(l.AllocationID) || l.Epoch == 0 || l.Placement != p.config.Name || l.InstructionDigest != p.config.InstructionDigest || l.Deadline.IsZero() || len(l.Allowances) == 0 || len(l.Allowances) > 32 {
		return errors.New("invalid Docker launch binding")
	}
	if (!initial && !validUUID(l.ThreadID)) || (initial && l.ThreadID != "") {
		return errors.New("invalid initial/resume native identity")
	}
	for name, a := range l.Allowances {
		if !resourceName.MatchString(name) || a.Units.Value < 0 || a.CostMicros.Value < 0 || (!a.Units.Bounded && a.Units.Value != 0) || (!a.CostMicros.Bounded && a.CostMicros.Value != 0) || (a.Units.Bounded && a.Units.Value == 0) || (a.CostMicros.Bounded && a.CostMicros.Value == 0) {
			return errors.New("invalid or exhausted Docker allowance")
		}
	}
	return nil
}

// Prepare restores a checkpoint, or for a first visit a captured Git workspace,
// into a fresh inert container. A first visit has no fabricated native UUID:
// the graceful stop binds the genuine one once, without changing the launch.
func (p *Placement) Prepare(ctx context.Context, l session.Launch, data []byte) (session.Incarnation, error) {
	unlock, err := p.lockOperation(l)
	if err != nil {
		return session.Incarnation{}, err
	}
	defer unlock()
	if err := p.validateLaunch(l); err != nil {
		return session.Incarnation{}, err
	}
	if l.CheckpointDigest == "" {
		if int64(len(data)) > p.config.MaxCheckpointBytes {
			return session.Incarnation{}, errors.New("invalid initial workspace artifact")
		}
	} else {
		meta, err := checkpoint.Inspect(data, p.config.MaxCheckpointBytes)
		if err != nil {
			return session.Incarnation{}, err
		}
		if meta.Harness != l.Harness || meta.ThreadID != l.ThreadID || meta.SHA256 != l.CheckpointDigest {
			return session.Incarnation{}, errors.New("docker checkpoint identity mismatch")
		}
	}
	return p.prepare(ctx, l, data)
}
func (p *Placement) prepare(ctx context.Context, l session.Launch, data []byte) (session.Incarnation, error) {
	l.Deadline = l.Deadline.UTC()
	l.IncludeIgnored = append([]string(nil), l.IncludeIgnored...)
	if !l.Deadline.After(time.Now()) {
		return session.Incarnation{}, errors.New("docker launch deadline expired")
	}
	r, err := p.load(l)
	if os.IsNotExist(err) {
		r = placementRecord{Version: recordVersion, ConfigDigest: p.digest, Launch: l, Execution: "spz-" + operationKey(l)[:32], InputDigest: fmt.Sprintf("%x", sha256.Sum256(data)), Phase: phaseCreating}
		if err = p.retain(operationKey(l)+".input", data); err != nil {
			return session.Incarnation{}, err
		}
		if err = p.save(r); err != nil {
			return session.Incarnation{}, err
		}
	} else if err != nil {
		return session.Incarnation{}, err
	}
	if r.InputDigest != fmt.Sprintf("%x", sha256.Sum256(data)) {
		return session.Incarnation{}, errors.New("docker prepare input changed")
	}
	if r.Phase == phaseCreating {
		if err = p.drivePrepare(ctx, &r, data); err != nil {
			return session.Incarnation{}, err
		}
	}
	if r.Phase != phasePrepared {
		return session.Incarnation{}, errors.New("docker operation cannot be prepared again")
	}
	return p.incarnation(r)
}
func (p *Placement) spec(r placementRecord, role string) ManagedSpec {
	limits, image, network := p.config.AgentLimits, p.config.NativeImageID, ""
	switch role {
	case roleAnchor:
		limits, image, network = p.config.AnchorLimits, p.config.AnchorImageID, p.config.NetworkName
	case roleHelper:
		limits, image, network = p.config.HelperLimits, p.config.HelperImageID, "none"
	case roleAgent:
		if r.Anchor != nil {
			network = "container:" + r.Anchor.ID
		}
	}
	spec := ManagedSpec{Name: r.Execution + "-" + role, Execution: r.Launch.AllocationID, Role: role, ImageID: image, NetworkMode: network, MemoryBytes: limits.MemoryBytes, CPUMilli: limits.CPUMilli, PIDs: limits.PIDs, Deadline: r.Launch.Deadline.Format(time.RFC3339Nano), GatewayIPv4: p.config.GatewayIPv4, GatewayPort: p.config.GatewayPort}
	encoded, _ := json.Marshal(r.Launch)
	spec.IntentDigest = fmt.Sprintf("%x", sha256.Sum256(append(encoded, []byte(r.ConfigDigest+r.InputDigest)...)))
	if role != roleAnchor {
		spec.Volume = r.Volume
	}
	return spec
}
func (p *Placement) resource(ctx context.Context, r *placementRecord, role string, target **Instance) error {
	spec := p.spec(*r, role)
	if *target == nil {
		i, err := p.engine.EnsureManaged(ctx, spec)
		if err != nil {
			return err
		}
		*target = &i
		if err = p.save(*r); err != nil {
			return err
		}
	} else if err := p.engine.VerifyManaged(ctx, **target, spec); err != nil {
		return err
	}
	return p.engine.Start(ctx, **target)
}
func (p *Placement) drivePrepare(ctx context.Context, r *placementRecord, data []byte) error {
	if !r.Launch.Deadline.After(time.Now()) {
		return errors.New("docker preparation deadline expired")
	}
	if r.Volume == nil {
		v, err := p.engine.EnsureVolume(ctx, r.Execution+"-workspace", r.Launch.AllocationID, p.config.VolumeBytes)
		if err != nil {
			return err
		}
		r.Volume = &v
		if err = p.save(*r); err != nil {
			return err
		}
	}
	if err := p.resource(ctx, r, roleAnchor, &r.Anchor); err != nil {
		return err
	}
	if err := p.resource(ctx, r, roleHelper, &r.Helper); err != nil {
		return err
	}
	if err := p.engine.PrepareArtifact(ctx, *r.Helper, p.config.Harness, data, p.config.MaxCheckpointBytes, p.config.InstructionDigest, r.fresh()); err != nil {
		return err
	}
	if err := p.resource(ctx, r, roleAgent, &r.Agent); err != nil {
		return err
	}
	inc, err := p.incarnation(*r)
	if err != nil {
		return err
	}
	access, err := p.authority.InitializeAllocation(ctx, r.Launch, inc)
	if err != nil {
		return err
	}
	contract := harness.NativeLaunch{Harness: p.config.Harness, Deadline: r.Launch.Deadline, ThreadID: r.Launch.ThreadID, Task: r.Launch.Task, Instructions: p.config.Instructions, InstructionDigest: p.config.InstructionDigest, Env: access.Env, Model: access.Model, Options: access.Options, ClientConfig: access.ClientConfig, TrustBundlePEM: access.TrustBundlePEM, UserInstructions: access.UserInstructions, RolloutMaxBytes: harness.NativeRolloutMaxBytes}
	if err := contract.Validate(time.Now()); err != nil {
		return err
	}
	input, err := json.Marshal(contract)
	if err != nil {
		return err
	}
	output, err := p.engine.NativeOperation(ctx, *r.Agent, harness.NativeConfigure, input)
	if err != nil {
		return err
	}
	if _, err = harness.DecodeNativeReply(output, harness.NativeConfigure, harness.NativePrepared, contract.ThreadID); err != nil {
		return err
	}
	r.Phase = phasePrepared
	return p.save(*r)
}
func (p *Placement) incarnation(r placementRecord) (session.Incarnation, error) {
	if r.Agent == nil || !objectID.MatchString(r.Agent.ID) || r.Agent.Execution != r.Launch.AllocationID {
		return session.Incarnation{}, errors.New("docker native incarnation unavailable")
	}
	return session.Incarnation{ExecutionID: r.Launch.AllocationID, ResourceUID: r.Agent.ID}, nil
}
func (p *Placement) bound(l session.Launch, i session.Incarnation) (placementRecord, error) {
	r, err := p.load(l)
	if err != nil {
		return r, err
	}
	actual, err := p.incarnation(r)
	if err != nil || actual != i {
		return r, errors.New("docker execution incarnation mismatch")
	}
	return r, nil
}
func (p *Placement) verify(ctx context.Context, r placementRecord) error {
	if r.Agent == nil || r.Anchor == nil || r.Helper == nil || r.Volume == nil {
		return errors.New("docker execution registration incomplete")
	}
	for _, role := range []string{roleAnchor, roleHelper, roleAgent} {
		i := r.Agent
		switch role {
		case roleAnchor:
			i = r.Anchor
		case roleHelper:
			i = r.Helper
		}
		if err := p.engine.VerifyManaged(ctx, *i, p.spec(r, role)); err != nil {
			return err
		}
	}
	anchor, err := p.engine.Observe(ctx, *r.Anchor)
	if err != nil || !anchor.Running || anchor.PID <= 0 {
		return errors.New("docker network anchor unavailable")
	}
	return p.engine.CheckAnchor(ctx, *r.Anchor, p.config.GatewayIPv4, p.config.GatewayPort)
}
func (p *Placement) Activate(ctx context.Context, l session.Launch, i session.Incarnation) error {
	unlock, err := p.lockOperation(l)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := p.bound(l, i)
	if err != nil {
		return err
	}
	return p.activate(ctx, &r, i)
}

// activate retries only the exact retained intent. The same trusted authority
// and fixed native launch gate are revalidated before recording activation.
func (p *Placement) activate(ctx context.Context, r *placementRecord, i session.Incarnation) error {
	if r.Phase != phasePrepared && r.Phase != phaseActivating && r.Phase != phaseActive {
		return errors.New("docker activation cannot reopen stopped allocation")
	}
	if !r.Launch.Deadline.After(time.Now()) {
		return errors.New("docker activation deadline expired")
	}
	if err := p.verify(ctx, *r); err != nil {
		return err
	}
	r.Phase = phaseActivating
	if err := p.save(*r); err != nil {
		return err
	}
	if err := p.authority.Activate(ctx, r.Launch, i); err != nil {
		return err
	}
	output, err := p.engine.NativeOperation(ctx, *r.Agent, harness.NativeActivate, nil)
	if err != nil {
		return err
	}
	if _, err = harness.DecodeInitialNativeReply(output, harness.NativeActivate, harness.NativeActive, r.Launch.ThreadID); err != nil {
		return err
	}
	r.Phase = phaseActive
	return p.save(*r)
}

func (p *Placement) Inspect(ctx context.Context, l session.Launch) (session.Observation, error) {
	o := session.Observation{Launch: l, State: session.PlacementUnknown}
	unlock, err := p.lockOperation(l)
	if err != nil {
		return o, err
	}
	defer unlock()
	r, err := p.load(l)
	if os.IsNotExist(err) {
		o.State = session.PlacementAbsent
		return o, nil
	}
	if err != nil {
		return o, err
	}
	if r.Phase == phaseCreating {
		o.State = session.PlacementPreparing
		if r.Agent != nil {
			o.Incarnation, err = p.incarnation(r)
		}
		return o, err
	}
	inc, err := p.incarnation(r)
	if err != nil {
		return o, err
	}
	o.Launch, o.Incarnation = r.Launch, inc
	if r.Phase == phaseActivating {
		if err = p.activate(ctx, &r, inc); err != nil {
			return o, err
		}
	}
	if r.ActualStopped {
		stopped, err := p.stopped(r)
		if err != nil {
			return o, err
		}
		o.State, o.Stopped = session.PlacementStopped, &stopped
		return o, nil
	}
	if err = p.verify(ctx, r); err != nil {
		return o, err
	}
	state, err := p.engine.Observe(ctx, *r.Agent)
	if err != nil {
		return o, err
	}
	if !state.Running || state.PID <= 0 {
		return o, errors.New("docker native execution ended without retained stop")
	}
	switch r.Phase {
	case phasePrepared:
		o.State = session.PlacementPrepared
	case phaseActive:
		o.State = session.PlacementActive
	}
	return o, nil
}

// Fenced reports, without changing any resource, whether this placement
// independently recorded that the launch's containers stopped or never
// existed. It is evidence that the allocation no longer consumes trust.
func (p *Placement) Fenced(l session.Launch) (bool, error) {
	unlock, err := p.lockOperation(l)
	if err != nil {
		return false, err
	}
	defer unlock()
	r, err := p.load(l)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return r.ActualStopped || r.Removed, nil
}
func (p *Placement) stopped(r placementRecord) (session.Stopped, error) {
	inc, err := p.incarnation(r)
	if err != nil {
		return session.Stopped{}, err
	}
	stopped := session.Stopped{Launch: r.Launch, Incarnation: inc, ActualStopped: r.ActualStopped, ThreadID: r.thread(), Receipt: r.Receipt, CheckpointDigest: r.CheckpointDigest}
	if r.CheckpointDigest != "" {
		data, err := p.read("out-"+r.CheckpointDigest+".checkpoint", p.config.MaxCheckpointBytes)
		if err != nil {
			return stopped, err
		}
		meta, err := checkpoint.Inspect(data, p.config.MaxCheckpointBytes)
		if err != nil || meta.SHA256 != r.CheckpointDigest || meta.Harness != r.Launch.Harness || meta.ThreadID != r.thread() {
			return stopped, errors.New("retained Docker checkpoint changed")
		}
		stopped.Checkpoint = data
	}
	return stopped, nil
}
func (p *Placement) Stop(ctx context.Context, l session.Launch, i session.Incarnation) (session.Stopped, error) {
	unlock, err := p.lockOperation(l)
	if err != nil {
		return session.Stopped{}, err
	}
	defer unlock()
	r, err := p.bound(l, i)
	if err != nil {
		return session.Stopped{}, err
	}
	if r.Phase == phaseStopped {
		// The stop is durable; removal failure is retried by the next call.
		_ = p.release(ctx, &r)
		return p.stopped(r)
	}
	if r.Phase == phaseAborted {
		return session.Stopped{}, errors.New("aborted Docker execution has no new move checkpoint")
	}
	r.Phase = phaseStopping
	if err = p.save(r); err != nil {
		return session.Stopped{}, err
	}
	// Preserve admitted capability work while input is gated and the native turn
	// drains. Graceful move must not revoke a held model/tool request mid-turn.
	if !r.ActualStopped {
		if err = p.quiesceNative(ctx, &r, i); err != nil {
			return session.Stopped{}, err
		}
	}
	if r.Launch.Harness == harness.NativeClaude && !r.NativeQuiet {
		return session.Stopped{}, errors.New("claude graceful completion acknowledgement unavailable")
	}
	if err = p.authority.Revoke(ctx, r.Launch, i); err != nil {
		if stopErr := p.engine.Stop(ctx, *r.Agent); stopErr == nil {
			r.ActualStopped = true
			_ = p.save(r)
		}
		return session.Stopped{}, err
	}
	if !r.ActualStopped {
		if err = p.engine.Stop(ctx, *r.Agent); err != nil {
			return session.Stopped{}, err
		}
		r.ActualStopped = true
		if err = p.save(r); err != nil {
			return session.Stopped{}, err
		}
	}

	if !validUUID(r.thread()) {
		return session.Stopped{}, errors.New("initial native UUID remains unknown")
	}
	if err = p.seal(ctx, &r, i); err != nil {
		stopped, stoppedErr := p.stopped(r)
		return stopped, errors.Join(fmt.Errorf("%w: %w", session.ErrUsageUnsealed, err), stoppedErr)
	}
	if r.CheckpointDigest == "" {
		if err = p.engine.VerifyManaged(ctx, *r.Helper, p.spec(r, roleHelper)); err != nil {
			return session.Stopped{}, err
		}
		data, err := p.engine.CaptureCheckpoint(ctx, *r.Helper, p.config.Harness, r.thread(), r.Launch.IncludeIgnored, p.config.MaxCheckpointBytes)
		if err != nil {
			return session.Stopped{}, err
		}
		if err = checkpoint.ValidateContinuity(data, p.config.Harness, p.config.InstructionDigest, p.config.MaxCheckpointBytes); err != nil {
			return session.Stopped{}, err
		}
		meta, err := checkpoint.Inspect(data, p.config.MaxCheckpointBytes)
		if err != nil || meta.Harness != r.Launch.Harness || meta.ThreadID != r.thread() {
			return session.Stopped{}, errors.New("docker capture identity mismatch")
		}
		if err = p.retain("out-"+meta.SHA256+".checkpoint", data); err != nil {
			return session.Stopped{}, err
		}
		r.CheckpointDigest = meta.SHA256
		if err = p.save(r); err != nil {
			return session.Stopped{}, err
		}
	}
	r.Phase = phaseStopped
	if err = p.save(r); err != nil {
		return session.Stopped{}, err
	}
	// Retain every receipt and checkpoint before asking the helper to unmount.
	// Cleanup failure cannot erase the durable source stop and sealed receipt.
	_ = p.engine.FinishHelper(ctx, *r.Helper)
	_ = p.engine.Stop(ctx, *r.Helper)
	_ = p.engine.Stop(ctx, *r.Anchor)
	_ = p.release(ctx, &r)
	return p.stopped(r)
}
func (p *Placement) seal(ctx context.Context, r *placementRecord, i session.Incarnation) error {
	if r.Receipt != nil {
		return nil
	}
	usage, err := p.authority.Seal(ctx, r.Launch, i)
	if err != nil {
		return err
	}
	if err = session.CheckUsage(r.Launch.Allowances, usage); err != nil {
		return err
	}
	r.Receipt = &session.Receipt{Launch: r.Launch, Incarnation: i, Usage: usage}
	return p.save(*r)
}
func (p *Placement) Abort(ctx context.Context, l session.Launch, i session.Incarnation) (session.Observation, error) {
	o := session.Observation{Launch: l, Incarnation: i, State: session.PlacementUnknown}
	unlock, err := p.lockOperation(l)
	if err != nil {
		return o, err
	}
	defer unlock()
	r, err := p.bound(l, i)
	if err != nil {
		return o, err
	}
	return p.abort(ctx, r, i)
}

func (p *Placement) abort(ctx context.Context, r placementRecord, i session.Incarnation) (session.Observation, error) {
	o := session.Observation{Launch: r.Launch, Incarnation: i, State: session.PlacementUnknown}
	var err error
	if r.Phase == phaseStopped || r.Phase == phaseAborted {
		// A retained receipt cannot substitute for current exact writer evidence.
		// Only this placement's own recorded removal replaces that evidence.
		if !r.Removed {
			if err = p.fenced(ctx, r); err != nil {
				return o, err
			}
			if r.Receipt == nil {
				// Retry existing accounting after a transient failure; Seal never
				// initializes a missing account or changes an existing receipt.
				_ = p.seal(ctx, &r, i)
			}
			_ = p.release(ctx, &r)
		}
		s, err := p.stopped(r)
		o.State, o.Stopped = session.PlacementStopped, &s
		return o, err
	}
	r.Phase = phaseAborting
	if err = p.save(r); err != nil {
		return o, err
	}
	if err = p.authority.Revoke(ctx, r.Launch, i); err != nil {
		if stopErr := p.engine.Stop(ctx, *r.Agent); stopErr == nil {
			r.ActualStopped = true
			_ = p.save(r)
		}
		return o, err
	}
	if err = p.engine.Stop(ctx, *r.Agent); err != nil {
		return o, err
	}
	r.ActualStopped = true
	if err = p.save(r); err != nil {
		return o, err
	}
	// Cancellation does not depend on a completed native turn or a fresh artifact.
	// Missing accounting is explicit; never turn it into zero consumption.
	_ = p.seal(ctx, &r, i)
	r.Phase = phaseAborted
	if err = p.save(r); err != nil {
		return o, err
	}
	s, err := p.stopped(r)
	o.Launch, o.State, o.Stopped = r.Launch, session.PlacementStopped, &s
	if r.Helper != nil {
		_ = p.engine.Stop(ctx, *r.Helper)
	}
	if r.Anchor != nil {
		_ = p.engine.Stop(ctx, *r.Anchor)
	}
	_ = p.release(ctx, &r)
	return o, err
}

// fenced confirms the exact agent container is stopped. Once the record says
// this placement is removing its containers, an absent agent is accepted as
// one it removed. Without that record, absence remains an error.
func (p *Placement) fenced(ctx context.Context, r placementRecord) error {
	if r.Removing {
		current, found, err := p.engine.lookup(ctx, r.Execution+"-"+roleAgent, r.Agent.Execution)
		if err != nil {
			return err
		}
		if !found || current.ID != r.Agent.ID {
			return nil
		}
	}
	return p.engine.Stop(ctx, *r.Agent)
}

// releasable reports a durable stop whose receipt and checkpoint are retained,
// or a cancelled preparation that never granted authority. Usage that is still
// unknown keeps the containers so a later Abort can retry sealing.
func (r placementRecord) releasable() bool {
	switch r.Phase {
	case phaseStopped, phaseAborted:
		return r.ActualStopped && r.Receipt != nil
	case phasePreparationAborting:
		return r.ActualStopped
	}
	return false
}

// release removes the visit's containers and workspace volume. It confirms
// every recorded container is stopped and saves Removing before the first
// removal, and Removed after the last, so each retry converges.
func (p *Placement) release(ctx context.Context, r *placementRecord) error {
	if r.Removed || !r.releasable() {
		return nil
	}
	roles := []string{roleAgent, roleHelper, roleAnchor}
	instances := []*Instance{r.Agent, r.Helper, r.Anchor}
	if !r.Removing {
		for _, instance := range instances {
			if instance != nil {
				if err := p.engine.Stop(ctx, *instance); err != nil {
					return err
				}
			}
		}
		r.Removing = true
		if err := p.save(*r); err != nil {
			return err
		}
	}
	for index, instance := range instances {
		if instance != nil {
			if err := p.engine.removeManaged(ctx, *instance, r.Execution+"-"+roles[index]); err != nil {
				return err
			}
		}
	}
	if r.Volume != nil {
		if err := p.engine.RemoveVolume(ctx, *r.Volume); err != nil {
			return err
		}
	}
	r.Removed = true
	return p.save(*r)
}

// AbortPreparation fences an allocation even when preparation failed before it
// could return a native incarnation. Lookup resolves lost create replies but
// never creates or starts resources. Records that never activated grant no
// authority; this cancellation returns no accounting or native continuity receipt.
func (p *Placement) AbortPreparation(ctx context.Context, l session.Launch) error {
	unlock, err := p.lockOperation(l)
	if err != nil {
		return err
	}
	defer unlock()
	r, err := p.load(l)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.Phase == phaseCreating {
		r.Phase = phasePreparationAborting
		if err = p.save(r); err != nil {
			return err
		}
	}
	if r.Removing && r.Phase == phasePreparationAborting {
		_ = p.release(ctx, &r)
		return nil
	}
	// Removed resources are answered from the record, never looked up again.
	for _, role := range []string{roleAnchor, roleHelper, roleAgent} {
		if r.Removing {
			break
		}
		target := &r.Anchor
		switch role {
		case roleHelper:
			target = &r.Helper
		case roleAgent:
			target = &r.Agent
		}
		if err = p.observeResource(ctx, &r, role, target); err != nil {
			return err
		}
	}
	if r.Phase != phasePreparationAborting && r.Agent != nil {
		i, err := p.incarnation(r)
		if err != nil {
			return err
		}
		_, err = p.abort(ctx, r, i)
		return err
	}
	// The creating phase never activated an allocation or the native gate.
	// Persisted cancellation denies every future prepare/activate retry.
	for _, instance := range []*Instance{r.Agent, r.Helper, r.Anchor} {
		if instance != nil {
			if err = p.engine.Stop(ctx, *instance); err != nil {
				return err
			}
		}
	}
	r.ActualStopped = true
	if err = p.save(r); err != nil {
		return err
	}
	_ = p.release(ctx, &r)
	return nil
}

func (p *Placement) observeResource(ctx context.Context, r *placementRecord, role string, target **Instance) error {
	spec := p.spec(*r, role)
	if *target == nil {
		i, found, err := p.engine.lookup(ctx, spec.Name, spec.Execution)
		if err != nil || !found {
			return err
		}
		if err = p.engine.VerifyManaged(ctx, i, spec); err != nil {
			return err
		}
		*target = &i
		return p.save(*r)
	}
	return p.engine.VerifyManaged(ctx, **target, spec)
}
func (p *Placement) Attach(ctx context.Context, l session.Launch, i session.Incarnation, input io.Reader, output io.Writer) error {
	unlock, err := p.lockOperation(l)
	if err != nil {
		return err
	}
	r, err := p.bound(l, i)
	if err != nil {
		unlock()
		return err
	}
	if r.Phase != phaseActive || !r.Launch.Deadline.After(time.Now()) {
		unlock()
		return errors.New("docker attachment is not active")
	}
	if err = p.verify(ctx, r); err != nil {
		unlock()
		return err
	}
	agent := *r.Agent
	unlock()
	deadline, cancel := context.WithDeadline(ctx, r.Launch.Deadline)
	defer cancel()
	return p.engine.AttachNative(deadline, agent, input, output)
}

func (p *Placement) quiesceNative(ctx context.Context, r *placementRecord, i session.Incarnation) error {
	if r.NativeQuiet {
		return nil
	}
	if err := p.verify(ctx, *r); err != nil {
		return err
	}
	state, err := p.engine.Observe(ctx, *r.Agent)
	if err != nil {
		return err
	}
	if state.Running {
		control := func(ctx context.Context, op harness.NativeOp) ([]byte, error) {
			return p.engine.NativeOperation(ctx, *r.Agent, op, nil)
		}
		bind := func(ctx context.Context, thread string) error {
			if err := p.authority.BindNative(ctx, r.Launch, i, thread); err != nil {
				return err
			}
			r.Thread = thread
			return p.save(*r)
		}
		_, err = harness.QuiesceNative(ctx, control, r.thread(), r.Launch.Deadline, bind)
		if errors.Is(err, harness.ErrQuiesceUnacknowledged) && r.Launch.Harness != harness.NativeClaude {
			// A lost Codex reply is resolved by the actual stopped container.
			observed, observeErr := p.engine.Observe(ctx, *r.Agent)
			if observeErr == nil && !observed.Running && observed.PID == 0 {
				err = nil
			}
		}
		if err != nil {
			return err
		}
	} else if r.Launch.Harness == harness.NativeClaude {
		return errors.New("claude graceful completion acknowledgement unavailable")
	}
	r.NativeQuiet = true
	return p.save(*r)
}
