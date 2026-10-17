package docker

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

type accountingAuthority struct {
	*fakeAuthority
	ledger budget.Ledger
	err    error
}

type corruptAccountingStore struct{}

const transientFixtureAccount = "transient"

func (corruptAccountingStore) Read(context.Context, budget.AccountKey) (*budget.Record, error) {
	return budget.DecodeRecord(`{"UnitsUsed":-1}`)
}

func (s corruptAccountingStore) Update(ctx context.Context, key budget.AccountKey, _ func(*budget.Record, bool) error) error {
	_, err := s.Read(ctx, key)
	return err
}

func (a *accountingAuthority) Seal(ctx context.Context, _ session.Launch, i session.Incarnation) (map[string]budget.TrustedUsage, error) {
	a.seals++
	if a.engine.containers[i.ResourceUID].State.Running {
		return nil, errors.New("seal before physical fence")
	}
	if a.err != nil {
		return nil, a.err
	}
	usage, err := a.ledger.Seal(ctx, budget.AccountKey(i.ExecutionID))
	if err != nil {
		return nil, err
	}
	return map[string]budget.TrustedUsage{modelFixtureCapability: usage}, nil
}

func TestAbortedDockerRetriesExistingAccountingWithoutInitializing(t *testing.T) {
	for _, name := range []string{transientFixtureAccount, missingFixtureAccount, "corrupt"} {
		t.Run(name, func(t *testing.T) {
			data := fixtureArtifact(t)
			l := fixtureLaunch(t, data)
			f := newFakeDocker()
			var ledger budget.Ledger = budget.NewMemoryLedger()
			if name == "corrupt" {
				ledger = budget.NewLedger(corruptAccountingStore{})
			}
			a := &accountingAuthority{fakeAuthority: &fakeAuthority{engine: f}, ledger: ledger}
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			p, err := OpenPlacement(dir, &Engine{execute: f.run}, fixtureConfig(), a)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			i, err := p.Prepare(t.Context(), l, data)
			if err != nil {
				t.Fatal(err)
			}
			key := budget.AccountKey(i.ExecutionID)
			if name == transientFixtureAccount {
				if err = ledger.Initialize(t.Context(), key); err != nil {
					t.Fatal(err)
				}
				r, reserveErr := ledger.Reserve(t.Context(), key, budget.UsageReservation{
					RequireExisting: true, MaxUnits: 100, MaxCostMicros: 1000, Units: 17, CostMicros: 23,
				})
				if reserveErr != nil {
					t.Fatal(reserveErr)
				}
				if err = ledger.Settle(t.Context(), r, budget.TrustedUsage{Units: 17, CostMicros: 23}); err != nil {
					t.Fatal(err)
				}
				a.err = errors.New("corrupt or unavailable stored account")
			}
			o, err := p.Abort(t.Context(), l, i)
			if err != nil || o.Stopped == nil || o.Stopped.Receipt != nil || !o.Stopped.ActualStopped {
				t.Fatalf("unknown receipt: %+v %v", o, err)
			}
			if err = p.Close(); err != nil {
				t.Fatal(err)
			}
			p, err = OpenPlacement(dir, &Engine{execute: f.run}, fixtureConfig(), a)
			if err != nil {
				t.Fatal(err)
			}
			a.err = nil
			o, err = p.Abort(t.Context(), l, i)
			if err != nil || o.Stopped == nil {
				t.Fatalf("replay: %+v %v", o, err)
			}
			if a.init != 1 {
				t.Fatal("cancellation replay initialized authority again")
			}
			if name != transientFixtureAccount {
				if o.Stopped.Receipt != nil {
					t.Fatal("unavailable account became zero receipt")
				}
				_, err = ledger.State(t.Context(), key)
				if err == nil || name == missingFixtureAccount && !errors.Is(err, budget.ErrAccountNotFound) {
					t.Fatalf("unavailable account recreated: %v", err)
				}
				return
			}
			if o.Stopped.Receipt == nil || o.Stopped.Receipt.Usage[modelFixtureCapability] != (budget.TrustedUsage{Units: 17, CostMicros: 23}) {
				t.Fatalf("receipt was not recovered: %+v", o)
			}
			seals := a.seals
			if _, err = p.Abort(t.Context(), l, i); err != nil || a.seals != seals {
				t.Fatalf("immutable receipt replaced: %v", err)
			}
			// Even a retained receipt cannot bypass a failed current engine fence.
			f.containers[i.ResourceUID].State.Running = true
			f.containers[i.ResourceUID].State.Pid = 11
			f.failStop = true
			if _, err = p.Abort(t.Context(), l, i); err == nil {
				t.Fatal("receipt replay bypassed exact physical fence")
			}
		})
	}
}

func TestClaudeGracefulStopRequiresPersistedAcknowledgement(t *testing.T) {
	for _, lostReply := range []bool{false, true} {
		name := "already-stopped"
		if lostReply {
			name = "lost-reply"
		}
		t.Run(name, func(t *testing.T) {
			f := newFakeDocker()
			a := &fakeAuthority{engine: f}
			cfg := fixtureConfig()
			cfg.Harness = harness.NativeClaude
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			p, err := OpenPlacement(dir, &Engine{execute: f.run}, cfg, a)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Close() })
			l := fixtureLaunch(t, fixtureArtifact(t))
			l.Harness, l.ThreadID, l.CheckpointDigest = harness.NativeClaude, "", ""
			i, err := p.Prepare(t.Context(), l, []byte("workspace-continuity"))
			if err != nil {
				t.Fatal(err)
			}
			if err = p.Activate(t.Context(), l, i); err != nil {
				t.Fatal(err)
			}
			if lostReply {
				f.lostQuiesceAck = true
			} else {
				c := f.containers[i.ResourceUID]
				c.State.Running, c.State.Pid, c.State.Status = false, 0, exitedStatus
			}
			if _, err = p.Stop(t.Context(), l, i); err == nil {
				t.Fatal("physical stop substituted for Claude completion")
			}
			if _, err = p.Stop(t.Context(), l, i); err == nil {
				t.Fatal("retry invented Claude completion")
			}
			r, err := p.load(l)
			if err != nil || r.NativeQuiet || r.CheckpointDigest != "" || a.seals != 0 {
				t.Fatalf("unconfirmed turn captured or sealed: %+v %v", r, err)
			}
			if _, err = p.Abort(t.Context(), l, i); err != nil {
				t.Fatalf("forced cancel unavailable: %v", err)
			}
		})
	}
}

func TestClaudeDurableAcknowledgementSurvivesCaptureFailure(t *testing.T) {
	f := newFakeDocker()
	f.failCapture = true
	a := &fakeAuthority{engine: f}
	cfg := fixtureConfig()
	cfg.Harness = harness.NativeClaude
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	p, err := OpenPlacement(dir, &Engine{execute: f.run}, cfg, a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	l := fixtureLaunch(t, fixtureArtifact(t))
	l.Harness, l.ThreadID, l.CheckpointDigest = harness.NativeClaude, "", ""
	i, err := p.Prepare(t.Context(), l, []byte("workspace-continuity"))
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Stop(t.Context(), l, i); err == nil {
		t.Fatal("capture failure hidden")
	}
	r, err := p.load(l)
	if err != nil || !r.NativeQuiet || !r.ActualStopped || r.CheckpointDigest != "" {
		t.Fatalf("acknowledgement was not durable: %+v %v", r, err)
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	p, err = OpenPlacement(dir, &Engine{execute: f.run}, cfg, a)
	if err != nil {
		t.Fatal(err)
	}
	before := len(f.calls)
	if _, err = p.Stop(t.Context(), l, i); err == nil {
		t.Fatal("capture retry failure hidden")
	}
	captured := false
	for _, call := range f.calls[before:] {
		if strings.Contains(call, "native-session quiesce") {
			t.Fatal("persisted acknowledgement required new native quiescence")
		}
		if strings.Contains(call, "checkpoint capture-stopped") {
			captured = true
		}
	}
	if !captured {
		t.Fatal("durable acknowledgement did not permit capture retry")
	}
}

func TestPartialInitialCancellationOnlyObservesAndFencesExistingResources(t *testing.T) {
	for _, failedRole := range []string{roleAnchor, roleHelper, roleAgent} {
		t.Run(failedRole, func(t *testing.T) {
			f := newFakeDocker()
			a := &fakeAuthority{engine: f}
			p := openFixture(t, t.TempDir(), f, a)
			original := p.engine.execute
			lostCreateReply := failedRole == roleAgent
			p.engine.execute = func(ctx context.Context, input []byte, max int, args []string) ([]byte, error) {
				if args[0] == createCommand && strings.HasSuffix(args[2], "-"+failedRole) {
					if lostCreateReply {
						_, _ = original(ctx, input, max, args)
					}
					return nil, errors.New("initial create failed")
				}
				return original(ctx, input, max, args)
			}
			l := fixtureLaunch(t, fixtureArtifact(t))
			l.ThreadID, l.CheckpointDigest = "", ""
			if _, err := p.Prepare(t.Context(), l, []byte("workspace-continuity")); err == nil {
				t.Fatal("partial preparation unexpectedly succeeded")
			}
			calls := len(f.calls)
			o, err := p.Inspect(t.Context(), l)
			if err != nil || o.State != session.PlacementPreparing || len(f.calls) != calls || a.init != 0 {
				t.Fatalf("inspection retried preparation: %+v %v", o, err)
			}
			created := f.next
			if err = p.AbortPreparation(t.Context(), l); err != nil {
				t.Fatal(err)
			}
			if f.next != created || a.init != 0 || a.activate != 0 || a.seals != 0 {
				t.Fatal("cancellation provisioned resources or invented initialized usage")
			}
			for _, c := range f.containers {
				if c.State.Running || c.State.Pid != 0 {
					t.Fatal("partial cancellation left a writer running")
				}
			}
			r, err := p.load(l)
			if err != nil || !r.ActualStopped || r.Receipt != nil || r.CheckpointDigest != "" {
				t.Fatalf("partial cancellation invented native state or usage: %+v %v", r, err)
			}
			if lostCreateReply && r.Agent == nil {
				t.Fatal("lost create acknowledgement was not bound to its exact container")
			}
			if err = p.AbortPreparation(t.Context(), l); err != nil {
				t.Fatal(err)
			}
			if _, err = p.Prepare(t.Context(), l, []byte("workspace-continuity")); err == nil {
				t.Fatal("cancelled preparation reopened")
			}
			if f.next != created {
				t.Fatal("cancellation retry created a replacement")
			}
		})
	}
}

func TestPartialInitialCancellationRejectsReplacementOrUnavailableEngine(t *testing.T) {
	f := newFakeDocker()
	p := openFixture(t, t.TempDir(), f, &fakeAuthority{engine: f})
	l := fixtureLaunch(t, fixtureArtifact(t))
	l.ThreadID, l.CheckpointDigest = "", ""
	original := p.engine.execute
	p.engine.execute = func(ctx context.Context, input []byte, max int, args []string) ([]byte, error) {
		if args[0] == createCommand && strings.HasSuffix(args[2], "-"+roleHelper) {
			return nil, errors.New("helper image unavailable")
		}
		return original(ctx, input, max, args)
	}
	if _, err := p.Prepare(t.Context(), l, []byte("workspace-continuity")); err == nil {
		t.Fatal("expected failed preparation")
	}
	r, err := p.load(l)
	if err != nil {
		t.Fatal(err)
	}
	anchor := f.containers[r.Anchor.ID]
	anchor.Config.Labels[ownerLabel] = "replacement"
	if err = p.AbortPreparation(t.Context(), l); err == nil || !anchor.State.Running {
		t.Fatal("cancellation accepted or stopped a replacement")
	}
	anchor.Config.Labels[ownerLabel] = l.AllocationID
	p.engine.execute = func(context.Context, []byte, int, []string) ([]byte, error) {
		return nil, errors.New("engine unavailable")
	}
	if err = p.AbortPreparation(t.Context(), l); err == nil {
		t.Fatal("engine failure became stopped evidence")
	}
	r, err = p.load(l)
	if err != nil || r.ActualStopped {
		t.Fatalf("unknown stop was accepted: %+v %v", r, err)
	}
	p.engine.execute = original
	if err = p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatal(err)
	}
}

func TestCoordinatorRecoversExactDockerActivationAfterLostReply(t *testing.T) {
	ctx := t.Context()
	data := fixtureArtifact(t)
	launch := fixtureLaunch(t, data)
	sourceEngine := newFakeDocker()
	sourceEngine.capture = data
	source := openFixture(t, t.TempDir(), sourceEngine, &fakeAuthority{engine: sourceEngine})
	const destination = "destination"
	f := newFakeDocker()
	a := &fakeAuthority{engine: f}
	cfg := fixtureConfig()
	cfg.Name = destination
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	p, err := OpenPlacement(dir, &Engine{execute: f.run}, cfg, a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	coordinatorDir := t.TempDir()
	if err = os.Chmod(coordinatorDir, 0700); err != nil {
		t.Fatal(err)
	}
	placements := map[string]session.Placement{localVolumeDriver: source, destination: p}
	c, err := session.Open(coordinatorDir, placements, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	// The coordinator runs the first visit on the source, then stops it.
	if _, err = c.Create(launch, data, ""); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Recover(ctx, launch.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if _, err = c.Finalize(ctx, launch.ExecutionID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	f.failActivationAck = true
	pending, err := c.Move(ctx, launch.ExecutionID, uuid.NewString(), destination)
	if err == nil || pending.Phase != session.PhaseActivating {
		t.Fatalf("lost activation acknowledgement: phase=%s error=%v", pending.Phase, err)
	}
	move := pending.Operations[len(pending.Operations)-1]
	want := *move.Incarnation
	if !f.containers[want.ResourceUID].State.Running || f.next != 3 {
		t.Fatal("native activation did not occur on the original incarnation")
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	p, err = OpenPlacement(dir, &Engine{execute: f.run}, cfg, a)
	if err != nil {
		t.Fatal(err)
	}
	placements[destination] = p
	c, err = session.Open(coordinatorDir, placements, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	// Retained activation intent does not bypass current gateway authority.
	a.activateErr = errors.New("current allocation admission denied")
	if _, err = c.Recover(ctx, launch.ExecutionID); err == nil {
		t.Fatal("recovery bypassed fresh authority admission")
	}
	if f.next != 3 {
		t.Fatal("failed recovery created a replacement")
	}
	a.activateErr = nil
	recovered, err := c.Recover(ctx, launch.ExecutionID)
	if err != nil || recovered.Active == nil || recovered.Active.Incarnation != want ||
		recovered.Phase != session.PhaseActive || recovered.PendingOperation != "" || f.next != 3 {
		t.Fatalf("same-incarnation recovery: record=%+v error=%v", recovered, err)
	}
	if recovered.Active.Launch.AllocationID != move.Launch.AllocationID {
		t.Fatal("recovery changed the retained allocation")
	}
}

func TestGracefulStopPreservesAdmittedTurnUntilQuiet(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "completed"
		if canceled {
			name = "canceled"
		}
		t.Run(name, func(t *testing.T) {
			data := fixtureArtifact(t)
			launch := fixtureLaunch(t, data)
			f := newFakeDocker()
			f.capture = data
			f.quiesceStarted = make(chan struct{})
			complete := make(chan struct{})
			f.completeTurn = complete
			a := &fakeAuthority{engine: f}
			p := openFixture(t, t.TempDir(), f, a)
			instance, err := p.Prepare(t.Context(), launch, data)
			if err != nil {
				t.Fatal(err)
			}
			if err = p.Activate(t.Context(), launch, instance); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { _, stopErr := p.Stop(ctx, launch, instance); done <- stopErr }()
			select {
			case <-f.quiesceStarted:
			case <-time.After(3 * time.Second):
				t.Fatal("stop did not reach gated pending turn")
			}
			if !a.active || a.revoked || a.seals != 0 || !f.containers[instance.ResourceUID].State.Running {
				t.Fatal("graceful move interrupted admitted capability work before native quiet")
			}
			if f.containers[instance.ResourceUID].nativeState != harness.NativeStopping {
				t.Fatal("new native input was not gated")
			}
			if canceled {
				cancel()
			} else {
				close(complete)
			}
			err = <-done
			if canceled {
				if err == nil || a.revoked || !a.active || a.seals != 0 {
					t.Fatal("failed graceful wait revoked or settled the held allocation")
				}
				if err = p.Activate(t.Context(), launch, instance); err == nil {
					t.Fatal("failed graceful wait reopened native input")
				}
				if _, err = p.Abort(t.Context(), launch, instance); err != nil {
					t.Fatal(err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !a.revoked || a.active || f.containers[instance.ResourceUID].State.Running || a.seals != 1 {
				t.Fatal("quiet/abort did not revoke, independently fence and settle")
			}
		})
	}
}
