package session

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/google/uuid"
)

const testMaxCheckpoint = int64(8 << 20)
const testThread = "01a126e8-288d-7350-9b52-627e16048343"
const testCapability = "model.inference"
const testLocal = "local"
const testRemote = "remote"
const testIgnoredFile = "needed.bin"

func TestCancelUnknownActivationWithExactKnownIncarnation(t *testing.T) {
	c, _, _, remote, start := setup(t)
	remote.lostActivate = true
	r, err := c.Move(context.Background(), start.Source.SessionID, uuid.NewString(), testRemote)
	if err == nil || r.Phase != PhaseActivating {
		t.Fatalf("expected retained activation intent: %+v %v", r, err)
	}
	op := r.Operations[len(r.Operations)-1]
	o := remote.records[op.Launch.AllocationID]
	o.State = PlacementUnknown
	remote.records[op.Launch.AllocationID] = o
	r, err = c.Cancel(context.Background(), r.ID, uuid.NewString(), nil)
	if err != nil || remote.aborted != 1 || r.Phase != PhaseUnknownUsage || r.CancelOperation == "" || r.Active == nil || !r.Active.ActualStopped {
		t.Fatalf("cancel unknown exact incarnation: %+v %v", r, err)
	}
	if r.Remaining[testCapability].Units.Value != 18 || r.Checkpoint.Metadata.SHA256 != start.Stopped.CheckpointDigest {
		t.Fatal("cancel refunded usage or replaced the committed checkpoint")
	}
}

// The coordinator distinguishes the Placement.Stop outcomes: a fenced writer
// with unsealed usage is unknown usage, any other failure is an unknown stop.
// Neither releases the allocation or prepares the destination.
func TestStopOutcomeSeparatesUnsealedUsageFromUnknownStop(t *testing.T) {
	for _, tc := range []struct {
		name       string
		unsealed   bool
		wantPhase  Phase
		wantFenced bool
	}{
		{name: "fenced writer, unsealed usage", unsealed: true, wantPhase: PhaseUnknownUsage, wantFenced: true},
		{name: "unreachable placement", wantPhase: PhaseUnknownStop},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _, local, remote, start := setup(t)
			r, err := c.Move(t.Context(), start.Source.SessionID, uuid.NewString(), testRemote)
			if err != nil {
				t.Fatal(err)
			}
			remote.unsealed, remote.unknownStop = tc.unsealed, !tc.unsealed
			r, err = c.Move(t.Context(), r.ID, uuid.NewString(), testLocal)
			if err == nil {
				t.Fatal("move succeeded without sealed source usage")
			}
			if r.Phase != tc.wantPhase {
				t.Errorf("phase = %s, want %s", r.Phase, tc.wantPhase)
			}
			if r.Active == nil || r.Active.ActualStopped != tc.wantFenced {
				t.Errorf("active visit = %+v, want fenced=%v", r.Active, tc.wantFenced)
			}
			if local.prepareCalls != 0 {
				t.Errorf("destination prepared %d times before the source was accounted", local.prepareCalls)
			}
		})
	}
}

// A new execution starts from a Git workspace with no native thread. The
// coordinator drives its first visit like any other, keeps the launch
// unchanged, and binds the genuine thread reported by the first stop.
func TestFirstVisitFromWorkspaceBindsGenuineThreadAtFirstStop(t *testing.T) {
	c, _, local, remote, start := setup(t)
	first := start.Source
	first.SessionID, first.OperationID, first.AllocationID = uuid.NewString(), uuid.NewString(), uuid.NewString()
	first.ThreadID, first.CheckpointDigest = "", ""
	workspace := []byte("captured git workspace")
	r, err := c.Create(first, workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != PhasePreparing || r.ThreadID != "" || r.Workspace == "" || r.Checkpoint != (Checkpoint{}) {
		t.Fatalf("new execution = %+v, want a preparing first visit without thread or checkpoint", r)
	}
	if _, err = c.Create(first, []byte("different workspace"), ""); err == nil {
		t.Fatal("retry replaced the retained workspace input")
	}
	if r, err = c.Recover(t.Context(), first.SessionID); err != nil || r.Phase != PhaseActive {
		t.Fatalf("first visit did not activate: %+v %v", r, err)
	}
	if local.activated != 1 {
		t.Fatalf("local activations = %d, want 1", local.activated)
	}
	r, err = c.Move(t.Context(), first.SessionID, uuid.NewString(), testRemote)
	if err != nil {
		t.Fatal(err)
	}
	if r.ThreadID != testThread {
		t.Errorf("execution thread = %q, want the stopped visit's %q", r.ThreadID, testThread)
	}
	if !SameLaunch(r.Visits[0].Launch, first) || r.Visits[0].Launch.ThreadID != "" {
		t.Errorf("first visit launch changed after binding: %+v", r.Visits[0].Launch)
	}
	if r.Active.Launch.ThreadID != testThread || remote.activated != 1 {
		t.Errorf("destination launch = %+v, want the bound thread", r.Active.Launch)
	}
}

type fakePlacement struct {
	data                                  []byte
	digest                                string
	records                               map[string]Observation
	prepared, activated, stopped, aborted int
	lostPrepare, lostActivate, lostStop   bool
	partialPrepare                        bool
	prepareCalls                          int
	unknownStop                           bool
	missingUsage                          bool
	unsealed                              bool
	corruptReceipt                        func(*Receipt)
	charge                                budget.TrustedUsage
}

func newPlacement(data []byte, digest string) *fakePlacement {
	return &fakePlacement{data: data, digest: digest, records: map[string]Observation{}, charge: budget.TrustedUsage{Units: 5, CostMicros: 7}}
}
func (p *fakePlacement) Prepare(_ context.Context, l Launch, data []byte) (Incarnation, error) {
	p.prepareCalls++
	if l.CheckpointDigest == "" && len(data) == 0 || l.CheckpointDigest != "" && (l.CheckpointDigest != p.digest || string(data) != string(p.data)) {
		return Incarnation{}, errors.New("wrong checkpoint")
	}
	if o, ok := p.records[l.AllocationID]; ok {
		if !SameLaunch(l, o.Launch) {
			return Incarnation{}, errors.New("conflict")
		}
		if o.State == PlacementPreparing {
			o.State = PlacementPrepared
			p.records[l.AllocationID] = o
		}
		return o.Incarnation, nil
	}
	p.prepared++
	i := Incarnation{ExecutionID: l.AllocationID, ResourceUID: uuid.NewString()}
	p.records[l.AllocationID] = Observation{Launch: l, Incarnation: i, State: PlacementPrepared}
	if p.partialPrepare {
		o := p.records[l.AllocationID]
		o.State = PlacementPreparing
		p.records[l.AllocationID] = o
		return Incarnation{}, errors.New("restore helper failed after resource preparation")
	}
	if p.lostPrepare {
		p.lostPrepare = false
		return Incarnation{}, errors.New("lost prepare reply")
	}
	return i, nil
}
func (p *fakePlacement) Activate(_ context.Context, l Launch, i Incarnation) error {
	o, ok := p.records[l.AllocationID]
	if !ok || !SameLaunch(l, o.Launch) || o.Incarnation != i {
		return errors.New("unknown execution")
	}
	if o.State == PlacementActive {
		return nil
	}
	if o.State != PlacementPrepared {
		return errors.New("not inert")
	}
	p.activated++
	o.State = PlacementActive
	p.records[l.AllocationID] = o
	if p.lostActivate {
		p.lostActivate = false
		return errors.New("lost activation reply")
	}
	return nil
}
func (p *fakePlacement) Inspect(_ context.Context, l Launch) (Observation, error) {
	if o, ok := p.records[l.AllocationID]; ok {
		return o, nil
	}
	return Observation{Launch: l, State: PlacementAbsent}, nil
}
func (p *fakePlacement) Stop(_ context.Context, l Launch, i Incarnation) (Stopped, error) {
	if p.unknownStop {
		return Stopped{}, errors.New("unreachable placement")
	}
	o, ok := p.records[l.AllocationID]
	if !ok || o.Incarnation != i || !SameLaunch(o.Launch, l) {
		return Stopped{}, errors.New("stale execution")
	}
	if p.unsealed {
		return Stopped{Launch: l, Incarnation: i, ActualStopped: true, ThreadID: l.ThreadID}, ErrUsageUnsealed
	}
	if o.Stopped != nil {
		return *o.Stopped, nil
	}
	p.stopped++
	r := &Receipt{Launch: l, Incarnation: i, Usage: map[string]budget.TrustedUsage{testCapability: p.charge}}
	if p.corruptReceipt != nil {
		p.corruptReceipt(r)
	}
	if p.missingUsage {
		r = nil
	}
	stopped := Stopped{Launch: l, Incarnation: i, ActualStopped: true, ThreadID: cmp.Or(l.ThreadID, testThread), Receipt: r, Checkpoint: p.data, CheckpointDigest: p.digest}
	o.State = PlacementStopped
	o.Stopped = &stopped
	p.records[l.AllocationID] = o
	if p.lostStop {
		p.lostStop = false
		return Stopped{}, errors.New("lost stop reply")
	}
	return stopped, nil
}
func (p *fakePlacement) Abort(_ context.Context, l Launch, i Incarnation) (Observation, error) {
	o, ok := p.records[l.AllocationID]
	if !ok || o.Incarnation != i || !SameLaunch(l, o.Launch) {
		return Observation{}, errors.New("stale abort")
	}
	p.aborted++
	o.State = PlacementStopped
	o.Stopped = nil
	p.records[l.AllocationID] = o
	return o, nil
}
func artifact(t *testing.T) ([]byte, string) {
	t.Helper()
	repo, home := t.TempDir(), t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@invalid"}
		if data, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("fixture Git: %v %s", err, data)
		}
	}
	run("init", "--quiet", "--template=", "-b", "main")
	if err := os.WriteFile(filepath.Join(repo, "note"), []byte("initial\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("add", ".")
	run("commit", "--quiet", "-m", "Fixture")
	state := harness.CodexState{ThreadID: testThread, Path: "sessions/2026/10/10/rollout-2026-10-10T18-41-51-" + testThread + ".jsonl", Rollout: []byte(`{"type":"session_meta","payload":{"id":"01a126e8-288d-7350-9b52-627e16048343","cli_version":"0.161.0","cwd":"/workspace"}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"fixture-turn"}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"fixture-turn"}}
`)}
	if err := harness.RestoreCodexState(home, state, testMaxCheckpoint); err != nil {
		t.Fatal(err)
	}
	data, meta, err := checkpoint.CaptureStopped(context.Background(), harness.NativeCodex, home, repo, testThread, nil, testMaxCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	return data, meta.SHA256
}

// Fixture allowance and the first visit's sealed usage. Every test that starts
// from setup sees testAllowance minus seedUsage remaining.
const (
	testAllowanceUnits = 20
	testAllowanceCost  = 100
	seedUnits          = 2
	seedCost           = 3
)

// seed is the stopped first visit that setup leaves behind.
type seed struct {
	Source  Launch
	Stopped Stopped
	Input   []byte
}

// setup returns an idle execution whose first local visit ran and stopped with
// seedUsage sealed, so later moves start from a committed checkpoint. Placement
// counters are reset afterwards.
func setup(t *testing.T, includeIgnored ...string) (*Coordinator, string, *fakePlacement, *fakePlacement, seed) {
	t.Helper()
	data, digest := artifact(t)
	local, remote := newPlacement(data, digest), newPlacement(data, digest)
	dir := filepath.Join(t.TempDir(), "journal")
	c, err := Open(dir, map[string]Placement{testLocal: local, testRemote: remote}, testMaxCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	source := Launch{Harness: harness.NativeCodex, SessionID: uuid.NewString(), OperationID: uuid.NewString(), AllocationID: uuid.NewString(), Epoch: 1, Placement: testLocal, ThreadID: testThread, CheckpointDigest: digest, InstructionDigest: strings.Repeat("a", 64), Deadline: time.Now().Add(time.Hour).UTC(),
		Allowances: map[string]Allowance{testCapability: {Units: Limit{Bounded: true, Value: testAllowanceUnits}, CostMicros: Limit{Bounded: true, Value: testAllowanceCost}}}}
	source.IncludeIgnored = includeIgnored
	return c, dir, local, remote, seedExecution(t, c, local, source, data)
}

// seedExecution creates source's execution, runs its first visit on local and
// stops it with seedUsage, then resets local's counters.
func seedExecution(t *testing.T, c *Coordinator, local *fakePlacement, source Launch, data []byte) seed {
	t.Helper()
	if _, err := c.Create(source, data, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Recover(t.Context(), source.SessionID); err != nil {
		t.Fatal(err)
	}
	charge := local.charge
	local.charge = budget.TrustedUsage{Units: seedUnits, CostMicros: seedCost}
	r, err := c.Finalize(t.Context(), source.SessionID, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	local.charge = charge
	local.prepared, local.activated, local.stopped, local.prepareCalls = 0, 0, 0, 0
	first := r.Visits[0]
	stopped := Stopped{Launch: first.Launch, Incarnation: first.Incarnation, ActualStopped: true, ThreadID: r.ThreadID, Receipt: first.Receipt, Checkpoint: data, CheckpointDigest: first.CheckpointDigest}
	return seed{Source: source, Stopped: stopped, Input: data}
}

func TestIgnoredSelectionSurvivesEveryMoveAndCannotAmendSeed(t *testing.T) {
	c, _, _, _, start := setup(t, testIgnoredFile)
	for _, destination := range []string{testRemote, testLocal, testRemote} {
		r, err := c.Move(t.Context(), start.Source.SessionID, uuid.NewString(), destination)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(r.IncludeIgnored, ",") != testIgnoredFile || strings.Join(r.Active.Launch.IncludeIgnored, ",") != testIgnoredFile {
			t.Fatal("move dropped the approved ignored file selection")
		}
		for _, visit := range r.Visits {
			if strings.Join(visit.Launch.IncludeIgnored, ",") != testIgnoredFile || strings.Join(visit.Receipt.Launch.IncludeIgnored, ",") != testIgnoredFile {
				t.Fatal("retained sealed visit lost the ignored file selection")
			}
		}
	}
	start.Source.IncludeIgnored = []string{"other.bin"}
	if _, err := c.Create(start.Source, start.Input, ""); err == nil {
		t.Fatal("import retry changed immutable ignored file selection")
	}
}
func TestRoundTripMaintainsOriginalAuthorityAndReceipts(t *testing.T) {
	c, dir, local, remote, start := setup(t)
	ctx := context.Background()
	remoteOp, localOp := uuid.NewString(), uuid.NewString()
	r, err := c.Move(ctx, start.Source.SessionID, remoteOp, testRemote)
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != PhaseActive || r.Epoch != 2 || r.Remaining[testCapability].Units.Value != 18 || r.Remaining[testCapability].CostMicros.Value != 97 {
		t.Fatalf("first placement: %+v", r)
	}
	if !r.Active.Launch.Deadline.Equal(start.Source.Deadline) || r.Active.Launch.InstructionDigest != start.Source.InstructionDigest || r.Active.Launch.ThreadID != testThread {
		t.Fatal("authority widened or identity replaced")
	}
	if _, err = c.Move(ctx, r.ID, remoteOp, testRemote); err != nil {
		t.Fatal(err)
	}
	if remote.activated != 1 {
		t.Fatal("replay launched another execution")
	}
	r, err = c.Move(ctx, r.ID, localOp, testLocal)
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != PhaseActive || r.Epoch != 3 || r.Remaining[testCapability].Units.Value != 13 || r.Remaining[testCapability].CostMicros.Value != 90 || remote.stopped != 1 || local.activated != 1 {
		t.Fatalf("return trip: %+v", r)
	}
	if _, err = c.Create(start.Source, start.Input, ""); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, map[string]Placement{testLocal: local, testRemote: remote}, testMaxCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.Inspect(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Epoch != 3 || got.Remaining[testCapability].Units.Value != 13 || got.Active.Incarnation != r.Active.Incarnation {
		t.Fatalf("durable recovery: %+v", got)
	}
	data, err := os.ReadFile(filepath.Join(dir, got.Checkpoint.Path))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := checkpoint.Inspect(data, testMaxCheckpoint)
	if err != nil || meta.SHA256 != start.Stopped.CheckpointDigest {
		t.Fatal("lost committed checkpoint")
	}
}
func TestLostRepliesRecoverSameAllocation(t *testing.T) {
	for _, stage := range []string{"prepare", "activate"} {
		t.Run(stage, func(t *testing.T) {
			c, dir, local, remote, start := setup(t)
			if stage == "prepare" {
				remote.lostPrepare = true
			} else {
				remote.lostActivate = true
			}
			r, err := c.Move(context.Background(), start.Source.SessionID, uuid.NewString(), testRemote)
			if err == nil {
				t.Fatal("fixture did not lose reply")
			}
			allocation := r.Operations[len(r.Operations)-1].Launch.AllocationID
			if err = c.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(dir, map[string]Placement{testLocal: local, testRemote: remote}, testMaxCheckpoint)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			got, err := reopened.Recover(context.Background(), r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Active == nil || got.Active.Launch.AllocationID != allocation || got.Epoch != 2 || remote.prepared != 1 || remote.activated != 1 {
				t.Fatalf("duplicate/lost allocation: %+v", got)
			}
		})
	}
}

func TestPartialPreparationRecoversThroughExplicitPrepareWithoutReplacingIncarnation(t *testing.T) {
	c, _, _, remote, start := setup(t)
	remote.partialPrepare = true
	r, err := c.Move(t.Context(), start.Source.SessionID, uuid.NewString(), testRemote)
	if err == nil || r.Phase != PhasePreparing {
		t.Fatalf("expected retained incomplete preparation: %+v %v", r, err)
	}
	allocation := r.Operations[len(r.Operations)-1].Launch.AllocationID
	exact := remote.records[allocation].Incarnation
	remote.partialPrepare = false
	r, err = c.Recover(t.Context(), r.ID)
	if err != nil || r.Phase != PhaseActive || r.Active == nil || r.Active.Incarnation != exact ||
		remote.prepareCalls != 2 || remote.prepared != 1 || remote.activated != 1 {
		t.Fatalf("partial preparation recovery changed or admitted unprepared resources: %+v %v", r, err)
	}
}

func TestPartialPreparationCancellationNeverRetriesFailedRestoreOrRefundsUnknownUsage(t *testing.T) {
	for name, identityKnown := range map[string]bool{"observed": true, "unobserved": false} {
		t.Run(name, func(t *testing.T) {
			c, _, _, remote, start := setup(t)
			remote.partialPrepare = true
			r, err := c.Move(t.Context(), start.Source.SessionID, uuid.NewString(), testRemote)
			if err == nil {
				t.Fatal("incomplete preparation hidden")
			}
			allocation := r.Operations[len(r.Operations)-1].Launch.AllocationID
			if !identityKnown {
				o := remote.records[allocation]
				o.Incarnation = Incarnation{}
				remote.records[allocation] = o
			}
			r, err = c.Cancel(t.Context(), r.ID, uuid.NewString(), nil)
			if identityKnown {
				if err != nil || remote.aborted != 1 || r.Phase != PhaseUnknownUsage || r.Active == nil || !r.Active.ActualStopped {
					t.Fatalf("exact partial preparation not stopped: %+v %v", r, err)
				}
			} else if err == nil || remote.aborted != 0 || r.Phase != PhaseUnknownStop {
				t.Fatalf("unobserved resource accepted as stopped: %+v %v", r, err)
			}
			if remote.prepareCalls != 1 || remote.activated != 0 || r.Remaining[testCapability].Units.Value != 18 ||
				r.Checkpoint.Metadata.SHA256 != start.Stopped.CheckpointDigest {
				t.Fatal("cancellation retried restore, activated destination, refunded unknown usage or replaced checkpoint")
			}
		})
	}
}
func TestUnknownStopOrUsagePreventsNextWriter(t *testing.T) {
	cases := map[string]func(*fakePlacement){
		"unreachable":  func(p *fakePlacement) { p.unknownStop = true },
		"missing seal": func(p *fakePlacement) { p.missingUsage = true },
		"negative": func(p *fakePlacement) {
			p.corruptReceipt = func(r *Receipt) { r.Usage[testCapability] = budget.TrustedUsage{Units: -1} }
		},
		"excess": func(p *fakePlacement) {
			p.corruptReceipt = func(r *Receipt) { r.Usage[testCapability] = budget.TrustedUsage{Units: 19} }
		},
		"wrong allocation": func(p *fakePlacement) {
			p.corruptReceipt = func(r *Receipt) { r.Launch.AllocationID = uuid.NewString() }
		},
		"incomplete": func(p *fakePlacement) {
			p.corruptReceipt = func(r *Receipt) { r.Usage = map[string]budget.TrustedUsage{} }
		},
	}
	for name, configure := range cases {
		t.Run(name, func(t *testing.T) {
			c, _, local, remote, start := setup(t)
			r, err := c.Move(context.Background(), start.Source.SessionID, uuid.NewString(), testRemote)
			if err != nil {
				t.Fatal(err)
			}
			configure(remote)
			got, err := c.Move(context.Background(), r.ID, uuid.NewString(), testLocal)
			if err == nil {
				t.Fatal("accepted unknown usage/stop")
			}
			if got.Active == nil || got.Remaining[testCapability].Units.Value != 18 || local.prepared != 0 || got.Phase != PhaseUnknownStop && got.Phase != PhaseUnknownUsage {
				t.Fatalf("released encumbered capacity: %+v", got)
			}
		})
	}
}
func TestCancellationPersistsBeforeAbortAndNeverRefundsUnknownUsage(t *testing.T) {
	c, _, local, remote, start := setup(t)
	r, err := c.Move(context.Background(), start.Source.SessionID, uuid.NewString(), testRemote)
	if err != nil {
		t.Fatal(err)
	}
	cancelOp := uuid.NewString()
	r, err = c.Cancel(context.Background(), r.ID, cancelOp, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Phase != PhaseUnknownUsage || r.CancelOperation == "" || r.Active == nil || !r.Active.ActualStopped || r.Remaining[testCapability].Units.Value != 18 || remote.aborted != 1 {
		t.Fatalf("cancel refund/fence: %+v", r)
	}
	if _, err = c.Move(context.Background(), r.ID, uuid.NewString(), testLocal); err == nil {
		t.Fatal("activated after cancellation")
	}
	if local.prepared != 0 {
		t.Fatal("destination prepared after cancellation")
	}
	if _, err = c.Cancel(context.Background(), r.ID, uuid.NewString(), nil); err == nil {
		t.Fatal("accepted conflicting cancel")
	}
}
func TestLimitsAndReplayIntent(t *testing.T) {
	c, _, _, remote, start := setup(t)
	changed := start
	changed.Source = sourceCopy(start.Source)
	changed.Source.Allowances[testCapability] = Allowance{Units: Limit{Bounded: true, Value: 21}, CostMicros: Limit{Bounded: true, Value: 100}}
	if _, err := c.Create(changed.Source, start.Input, ""); err == nil {
		t.Fatal("accepted widened seed")
	}
	op := uuid.NewString()
	r, err := c.Move(context.Background(), start.Source.SessionID, op, testRemote)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Move(context.Background(), r.ID, op, testLocal); err == nil {
		t.Fatal("accepted replay destination change")
	}
	remote.charge = budget.TrustedUsage{Units: 18, CostMicros: 97}
	got, err := c.Move(context.Background(), r.ID, uuid.NewString(), testLocal)
	if err == nil {
		t.Fatal("exhausted bounded allowance became unlimited")
	}
	if got.Remaining[testCapability].Units != (Limit{Bounded: true, Value: 0}) || got.Active != nil {
		t.Fatalf("exhaustion state: %+v", got)
	}
	if err := spendable(map[string]Allowance{testCapability: {Units: Limit{}, CostMicros: Limit{}}}); err != nil {
		t.Fatal("unbounded mistaken for exhausted")
	}
}
func sourceCopy(source Launch) Launch {
	source.Allowances = cloneAllowances(source.Allowances)
	return source
}
func TestPrivateJournalAndCorruptionFailClosed(t *testing.T) {
	c, dir, local, remote, start := setup(t)
	if other, err := Open(dir, map[string]Placement{testLocal: local, testRemote: remote}, testMaxCheckpoint); err == nil {
		_ = other.Close()
		t.Fatal("two journal owners")
	}
	for _, path := range []string{dir, filepath.Join(dir, "checkpoints"), filepath.Join(dir, "owner.lock"), filepath.Join(dir, start.Source.SessionID+".json")} {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0077 != 0 {
			t.Fatal("private journal became public")
		}
	}
	record, err := c.Inspect(start.Source.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	record.Remaining[testCapability] = Allowance{}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, record.ID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Recover(context.Background(), record.ID); err == nil {
		t.Fatal("corrupt journal refunded capacity")
	}
	if remote.prepared != 0 {
		t.Fatal("corrupt journal activated placement")
	}
}

func TestConcurrentReplayHasOneWriter(t *testing.T) {
	c, _, _, remote, start := setup(t)
	op := uuid.NewString()
	results := make(chan error, 2)
	for range 2 {
		go func() { _, err := c.Move(context.Background(), start.Source.SessionID, op, testRemote); results <- err }()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	r, err := c.Inspect(start.Source.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if remote.prepared != 1 || remote.activated != 1 || r.Active == nil || r.Epoch != 2 {
		t.Fatalf("concurrent replay duplicated writer: %+v", r)
	}
}
func TestUnknownUsageCancellationSurvivesReopen(t *testing.T) {
	c, dir, local, remote, start := setup(t)
	r, err := c.Move(context.Background(), start.Source.SessionID, uuid.NewString(), testRemote)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Cancel(context.Background(), r.ID, uuid.NewString(), nil); err != nil {
		t.Fatal(err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir, map[string]Placement{testLocal: local, testRemote: remote}, testMaxCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	got, err := reopened.Recover(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != PhaseUnknownUsage || got.CancelOperation == "" || got.Active == nil || !got.Active.ActualStopped || got.Remaining[testCapability].Units.Value != 18 {
		t.Fatalf("cancel recovery refunded unknown usage: %+v", got)
	}
}
func TestUnboundedKeyChangeIsStillAConflict(t *testing.T) {
	if sameAllowances(map[string]Allowance{"model.inference": {}}, map[string]Allowance{"github.pullRequest": {}}) {
		t.Fatal("unbounded key change authorized another capability")
	}
	if CheckUsage(map[string]Allowance{"model.inference": {}}, map[string]budget.TrustedUsage{"github.pullRequest": {}}) == nil {
		t.Fatal("zero-charge receipt key change accepted")
	}
}

func TestLostStopReplyLeavesEscrowUntilSameReceiptRecovered(t *testing.T) {
	c, _, local, remote, start := setup(t)
	r, err := c.Move(context.Background(), start.Source.SessionID, uuid.NewString(), testRemote)
	if err != nil {
		t.Fatal(err)
	}
	remote.lostStop = true
	r, err = c.Move(context.Background(), r.ID, uuid.NewString(), testLocal)
	if err == nil {
		t.Fatal("fixture did not lose stop reply")
	}
	if r.Phase != PhaseUnknownStop || r.Active == nil || r.Remaining[testCapability].Units.Value != 18 || local.prepared != 0 {
		t.Fatalf("lost stop refunded escrow: %+v", r)
	}
	got, err := c.Recover(context.Background(), r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Phase != PhaseActive || got.Active.Launch.Placement != testLocal || got.Remaining[testCapability].Units.Value != 13 || remote.stopped != 1 || local.activated != 1 {
		t.Fatalf("stop recovery repeated usage/activation: %+v", got)
	}
}
func TestCheckpointDigestMismatchCannotActivateDestination(t *testing.T) {
	c, _, local, remote, start := setup(t)
	r, err := c.Move(context.Background(), start.Source.SessionID, uuid.NewString(), testRemote)
	if err != nil {
		t.Fatal(err)
	}
	remote.digest = strings.Repeat("b", 64)
	got, err := c.Move(context.Background(), r.ID, uuid.NewString(), testLocal)
	if err == nil {
		t.Fatal("accepted checkpoint with false digest")
	}
	if got.Active == nil || local.prepared != 0 || got.Remaining[testCapability].Units.Value != 18 {
		t.Fatalf("bad checkpoint released source escrow: %+v", got)
	}
}
func TestExpiredOriginalDeadlinePreventsPreparation(t *testing.T) {
	c, _, local, remote, start := setup(t)
	deadline := time.Now().Add(time.Second).UTC()
	source := start.Source
	source.SessionID, source.OperationID, source.AllocationID = uuid.NewString(), uuid.NewString(), uuid.NewString()
	source.Deadline = deadline
	expiring := seedExecution(t, c, local, source, start.Input)
	<-time.After(time.Until(deadline) + 10*time.Millisecond)
	if _, err := c.Move(context.Background(), expiring.Source.SessionID, uuid.NewString(), testRemote); err == nil {
		t.Fatal("move reset expired deadline")
	}
	if remote.prepared != 0 || remote.activated != 0 {
		t.Fatal("expired session prepared a destination")
	}
}
