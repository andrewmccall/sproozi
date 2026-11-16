package session

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/testutil"
	"github.com/google/uuid"
)

// fencingPlacement is a fakePlacement that can fence a preparation that
// never reported an incarnation, as the Docker placement can.
type fencingPlacement struct {
	*fakePlacement
	abortPreparations   int
	abortPreparationErr error
}

func (p *fencingPlacement) AbortPreparation(_ context.Context, l Launch) error {
	p.abortPreparations++
	if p.abortPreparationErr != nil {
		return p.abortPreparationErr
	}
	if o, ok := p.records[l.AllocationID]; ok {
		o.State = PlacementStopped
		p.records[l.AllocationID] = o
	}
	return nil
}

// lateCommand is a request that arrives after the deadline at an execution
// whose move or first visit has not made its destination active.
type lateCommand struct {
	name string
	run  func(*Coordinator, string) (Record, error)
	// want is the error the command reports once the execution is idle.
	want error
}

var lateCommands = []lateCommand{
	{"recover", func(c *Coordinator, id string) (Record, error) { return c.Recover(context.Background(), id) }, ErrDeadlinePassed},
	{"stop", func(c *Coordinator, id string) (Record, error) {
		return c.Finalize(context.Background(), id, uuid.NewString())
	}, nil},
	{"move", func(c *Coordinator, id string) (Record, error) {
		return c.Move(context.Background(), id, uuid.NewString(), testRemote)
	}, ErrDeadlinePassed},
}

// checkAbandoned asserts that r is idle, its pending operation ended without
// cancelling the execution, and the stored journal agrees.
func checkAbandoned(t *testing.T, c *Coordinator, r Record, err, want error) {
	t.Helper()
	if !errors.Is(err, want) && (want != nil || err != nil) {
		t.Fatalf("late command: %v, want %v", err, want)
	}
	if r.Phase != PhaseIdle || r.PendingOperation != "" || r.CancelOperation != "" || r.Active != nil {
		t.Fatalf("late command left %s pending %q cancel %q", r.Phase, r.PendingOperation, r.CancelOperation)
	}
	stored, err := c.Inspect(r.ID)
	if err != nil || stored.Phase != PhaseIdle {
		t.Fatalf("stored phase %s %v", stored.Phase, err)
	}
}

// resumeFrom starts a new execution from id's last checkpoint, as resume
// does, and activates it on the local placement.
func resumeFrom(t *testing.T, c *Coordinator, id string, template Launch) {
	t.Helper()
	last, err := c.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	data, err := c.Checkpoint(id)
	if err != nil {
		t.Fatal(err)
	}
	next := template
	next.ExecutionID, next.OperationID, next.AllocationID = uuid.NewString(), uuid.NewString(), uuid.NewString()
	next.Placement, next.ThreadID, next.CheckpointDigest = testLocal, last.ThreadID, last.Checkpoint.Metadata.SHA256
	next.Deadline = c.now().Add(time.Hour).UTC()
	if _, err = c.Create(next, data, ""); err != nil {
		t.Fatal(err)
	}
	r, err := c.Recover(context.Background(), next.ExecutionID)
	if err != nil || r.Phase != PhaseActive {
		t.Fatalf("resume from the kept checkpoint: %s %v", r.Phase, err)
	}
}

// A move that begins after the deadline stops its source, then ends instead
// of preparing a destination: the execution is idle with the checkpoint the
// stop committed, a stop is idempotent, and a new execution continues from
// it. This is "sproozi move ID --local" of a session on a cluster after its
// deadline.
func TestMoveAfterTheDeadlineStopsWithTheCheckpointKept(t *testing.T) {
	t.Parallel()
	c, _, local, remote, start := setup(t)
	id := start.Source.ExecutionID
	if r, err := c.Move(t.Context(), id, uuid.NewString(), testRemote); err != nil || r.Phase != PhaseActive {
		t.Fatalf("move to remote: %s %v", r.Phase, err)
	}
	c.now = func() time.Time { return start.Source.Deadline }
	op := uuid.NewString()
	inspections := local.inspections
	r, err := c.Move(t.Context(), id, op, testLocal)
	checkAbandoned(t, c, r, err, ErrDeadlinePassed)
	last := r.Visits[len(r.Visits)-1]
	if remote.stopped != 1 || last.Launch.Placement != testRemote || r.Checkpoint.Metadata.SHA256 != remote.digest ||
		local.prepareCalls != 0 || local.inspections != inspections {
		t.Fatalf("late move: remote stops %d, last visit at %s, local prepared %d inspected %d",
			remote.stopped, last.Launch.Placement, local.prepareCalls, local.inspections-inspections)
	}
	if ended := r.Operations[len(r.Operations)-1]; ended.ID != op || ended.Stage != stageCancelled || ended.Launch != nil {
		t.Fatalf("late move's operation: %+v", ended)
	}
	// A replay of the move reports the same outcome.
	if r, err = c.Move(t.Context(), id, op, testLocal); !errors.Is(err, ErrDeadlinePassed) || r.Phase != PhaseIdle {
		t.Fatalf("replayed late move: %s %v", r.Phase, err)
	}
	if r, err = c.Finalize(t.Context(), id, uuid.NewString()); err != nil || r.Phase != PhaseIdle {
		t.Fatalf("stop after the late move: %s %v", r.Phase, err)
	}
	if r, err = c.Recover(t.Context(), id); err != nil || r.Phase != PhaseIdle {
		t.Fatalf("recover after the late move: %s %v", r.Phase, err)
	}
	resumeFrom(t, c, id, start.Source)
}

// lateStart is a first visit whose destination is not yet active when its
// deadline passes. Its placement does not fence unreported preparations, so
// the coordinator inspects and aborts what it finds.
type lateStart struct {
	name string
	// interrupt leaves the created first visit unfinished.
	interrupt func(*testing.T, *Coordinator, *fakePlacement, string)
	phase     Phase
	// aborted is whether an incarnation exists that must be aborted.
	aborted bool
}

var lateStarts = []lateStart{
	{"prepare never called", func(*testing.T, *Coordinator, *fakePlacement, string) {}, PhasePreparing, false},
	{"prepare reply lost", func(t *testing.T, c *Coordinator, p *fakePlacement, id string) {
		p.lostPrepare = true
		recoverFails(t, c, id)
	}, PhasePreparing, true},
	{"prepare failed partway", func(t *testing.T, c *Coordinator, p *fakePlacement, id string) {
		p.partialPrepare = true
		recoverFails(t, c, id)
		p.partialPrepare = false
	}, PhasePreparing, true},
	{"activation refused", func(t *testing.T, c *Coordinator, p *fakePlacement, id string) {
		p.failActivate = true
		recoverFails(t, c, id)
		p.failActivate = false
	}, PhaseActivating, true},
}

func recoverFails(t *testing.T, c *Coordinator, id string) {
	t.Helper()
	if _, err := c.Recover(t.Context(), id); err == nil {
		t.Fatal("fixture did not interrupt the first visit")
	}
}

// createResumed retains a new execution from the seed's checkpoint, as
// resume does, without starting it.
func createResumed(t *testing.T, c *Coordinator, start seed) Launch {
	t.Helper()
	first := start.Source
	first.ExecutionID, first.OperationID, first.AllocationID = uuid.NewString(), uuid.NewString(), uuid.NewString()
	first.Deadline = time.Now().Add(time.Hour).UTC()
	if _, err := c.Create(first, start.Input, ""); err != nil {
		t.Fatal(err)
	}
	return first
}

// A resume interrupted before its activation and retried after its deadline:
// each of recover, stop and move ends the first visit, after aborting any
// incarnation it may have prepared, and the execution is idle with the
// checkpoint it started from. Recording a visit for an aborted incarnation
// keeps its sealed usage.
func TestFirstVisitInterruptedThenLateEndsIdle(t *testing.T) {
	t.Parallel()
	for _, start := range lateStarts {
		for _, command := range lateCommands {
			t.Run(start.name+"/"+command.name, func(t *testing.T) {
				t.Parallel()
				c, _, local, _, seeded := setup(t)
				first := createResumed(t, c, seeded)
				start.interrupt(t, c, local, first.ExecutionID)
				if r, err := c.Inspect(first.ExecutionID); err != nil || r.Phase != start.phase {
					t.Fatalf("interrupted first visit: %s %v", r.Phase, err)
				}
				local.abortSealed = true
				prepares := local.prepareCalls
				c.now = func() time.Time { return first.Deadline }
				r, err := command.run(c, first.ExecutionID)
				checkAbandoned(t, c, r, err, command.want)
				if local.prepareCalls != prepares || local.activated != 0 {
					t.Fatalf("late command prepared %d or activated %d", local.prepareCalls-prepares, local.activated)
				}
				if start.aborted != (local.aborted == 1) || start.aborted != (len(r.Visits) == 1) {
					t.Fatalf("aborted %d, visits %d", local.aborted, len(r.Visits))
				}
				if r.Checkpoint.Metadata.SHA256 != first.CheckpointDigest {
					t.Fatal("abandoned first visit lost the checkpoint it started from")
				}
				resumeFrom(t, c, first.ExecutionID, first)
			})
		}
	}
}

// Until the coordinator proves no writer can exist at the destination, the
// late start keeps its phase and says why; repeating the command finishes it.
// Nothing is recorded as stopped, and no usage is refunded.
func TestLateStartWaitsForProofThatNothingRuns(t *testing.T) {
	t.Parallel()
	for name, unproven := range map[string]func(*fakePlacement){
		"abort unreachable":   func(p *fakePlacement) { p.abortUnreachable = true },
		"usage unsealed":      func(*fakePlacement) {},
		"inspection unknown":  func(p *fakePlacement) { p.records = map[string]Observation{} },
		"incarnation unknown": func(p *fakePlacement) {},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, _, local, _, seeded := setup(t)
			first := createResumed(t, c, seeded)
			local.partialPrepare = true
			recoverFails(t, c, first.ExecutionID)
			local.partialPrepare = false
			allocation := first.AllocationID
			switch name {
			case "inspection unknown":
				o := local.records[allocation]
				o.State, o.Incarnation = PlacementUnknown, Incarnation{}
				unproven(local)
				local.records[allocation] = o
			case "incarnation unknown":
				o := local.records[allocation]
				o.Incarnation = Incarnation{}
				local.records[allocation] = o
			default:
				unproven(local)
			}
			c.now = func() time.Time { return first.Deadline }
			r, err := c.Recover(t.Context(), first.ExecutionID)
			if !errors.Is(err, ErrEndUnproven) || errors.Is(err, ErrDeadlinePassed) || r.Phase != PhasePreparing {
				t.Fatalf("unproven late start: %s %v", r.Phase, err)
			}
			if stored, err := c.Inspect(first.ExecutionID); err != nil || stored.Phase != PhasePreparing ||
				len(stored.Visits) != 0 || stored.Remaining[testCapability] != first.Allowances[testCapability] {
				t.Fatalf("unproven late start was recorded: %+v %v", stored, err)
			}
			if name != "usage unsealed" && name != "abort unreachable" {
				return
			}
			local.abortUnreachable, local.abortSealed = false, true
			r, err = c.Finalize(t.Context(), first.ExecutionID, uuid.NewString())
			checkAbandoned(t, c, r, err, nil)
		})
	}
}

// A placement that fences unreported preparations is asked to, without
// inspecting or aborting an incarnation, whether or not Prepare's reply was
// lost; a failure to fence leaves the start pending.
func TestLateStartFencesThePreparationWhenThePlacementCan(t *testing.T) {
	t.Parallel()
	_, _, local, remote, seeded := setup(t)
	fencing := &fencingPlacement{fakePlacement: local}
	other, err := Open(filepath.Join(testutil.PrivateDir(t), "journal"),
		map[string]Placement{testLocal: fencing, testRemote: remote}, testMaxCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })
	for _, lost := range []bool{false, true} {
		first := createResumed(t, other, seeded)
		local.lostPrepare = lost
		if lost {
			recoverFails(t, other, first.ExecutionID)
		}
		other.now = func() time.Time { return first.Deadline }
		fencing.abortPreparations, fencing.abortPreparationErr = 0, errors.New("docker unreachable")
		inspections := local.inspections
		if r, err := other.Recover(t.Context(), first.ExecutionID); err == nil || r.Phase != PhasePreparing {
			t.Fatalf("unfenced late start: %s %v", r.Phase, err)
		}
		fencing.abortPreparationErr = nil
		r, err := other.Recover(t.Context(), first.ExecutionID)
		checkAbandoned(t, other, r, err, ErrDeadlinePassed)
		if fencing.abortPreparations != 2 || local.inspections != inspections || local.aborted != 0 || len(r.Visits) != 0 {
			t.Fatalf("lost %v: fenced %d, inspected %d, aborted %d, visits %d", lost, fencing.abortPreparations,
				local.inspections-inspections, local.aborted, len(r.Visits))
		}
		other.now = time.Now
	}
}

// A late activation the placement cannot inspect, as Docker cannot once the
// prepared supervisor stopped itself at its deadline, still ends: the known
// incarnation is aborted, and the abort proves it stopped whatever Inspect
// said. Before the deadline the same Inspect failure is only reported.
func TestLateActivationEndsWhenInspectFails(t *testing.T) {
	t.Parallel()
	for _, command := range lateCommands {
		t.Run(command.name, func(t *testing.T) {
			t.Parallel()
			c, _, local, _, seeded := setup(t)
			first := createResumed(t, c, seeded)
			local.failActivate = true
			recoverFails(t, c, first.ExecutionID)
			local.failActivate = false
			local.inspectErr = errors.New("docker native execution ended without retained stop")
			if r, err := c.Recover(t.Context(), first.ExecutionID); err == nil || r.Phase != PhaseActivating || local.aborted != 0 {
				t.Fatalf("Inspect failure before the deadline: %s %v aborts %d", r.Phase, err, local.aborted)
			}
			local.abortSealed = true
			c.now = func() time.Time { return first.Deadline }
			r, err := command.run(c, first.ExecutionID)
			checkAbandoned(t, c, r, err, command.want)
			if local.aborted != 1 || len(r.Visits) != 1 || local.activated != 0 {
				t.Fatalf("late activation: aborted %d visits %d activated %d", local.aborted, len(r.Visits), local.activated)
			}
			local.inspectErr = nil
			resumeFrom(t, c, first.ExecutionID, first)
		})
	}
}

// A late activation that really happened is not ended: recovery finds the
// destination active and records it, as it would before the deadline.
func TestLateRecoveryRecordsAnActivationThatHappened(t *testing.T) {
	t.Parallel()
	c, _, local, _, seeded := setup(t)
	first := createResumed(t, c, seeded)
	local.lostActivate = true
	recoverFails(t, c, first.ExecutionID)
	c.now = func() time.Time { return first.Deadline }
	if r, err := c.Recover(t.Context(), first.ExecutionID); err != nil || r.Phase != PhaseActive || local.aborted != 0 {
		t.Fatalf("late recovery of an activation that happened: %s %v", r.Phase, err)
	}
}

// A journal the earlier coordinator left preparing past its deadline, with
// the destination's launch saved and the preparation refused, is read as it
// is and ended by the next recover, stop or move. Nothing migrates it.
func TestAlreadyStuckJournalTakesTheExit(t *testing.T) {
	t.Parallel()
	for _, command := range lateCommands {
		t.Run(command.name, func(t *testing.T) {
			t.Parallel()
			c, dir, local, remote, start := setup(t)
			id := start.Source.ExecutionID
			// The earlier coordinator saved the launch, then refused to
			// prepare it; a Prepare that fails before creating anything
			// leaves the same journal.
			remote.digest = "refused"
			if r, err := c.Move(t.Context(), id, uuid.NewString(), testRemote); err == nil || r.Phase != PhasePreparing ||
				r.Operations[len(r.Operations)-1].Launch == nil {
				t.Fatalf("stuck journal fixture: %s %v", r.Phase, err)
			}
			remote.digest = local.digest
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(dir, map[string]Placement{testLocal: local, testRemote: remote}, testMaxCheckpoint)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			reopened.now = func() time.Time { return start.Source.Deadline.Add(24 * time.Hour) }
			r, err := command.run(reopened, id)
			checkAbandoned(t, reopened, r, err, command.want)
			if remote.prepareCalls != 1 || remote.aborted != 0 || r.Checkpoint.Metadata.SHA256 != start.Stopped.CheckpointDigest {
				t.Fatalf("stuck journal: prepared %d aborted %d", remote.prepareCalls, remote.aborted)
			}
			resumeFrom(t, reopened, id, start.Source)
		})
	}
}
