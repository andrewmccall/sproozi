package session

import (
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/andrewmccall/sproozi/internal/testutil"
	"github.com/google/uuid"
)

const testRefusal = "AgentRun session-0 failed before its sandbox started: RuntimeInvalid: image is required"

// fencingSetup is setup with the local placement fencing preparations, as
// the Kubernetes placement does.
func fencingSetup(t *testing.T) (*Coordinator, *fencingPlacement, seed) {
	t.Helper()
	_, _, local, remote, seeded := setup(t)
	fencing := &fencingPlacement{fakePlacement: local}
	c, err := Open(filepath.Join(testutil.PrivateDir(t), "journal"),
		map[string]Placement{testLocal: fencing, testRemote: remote}, testMaxCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, fencing, seeded
}

// requireEnded checks that err reports the start ended by the placement's
// refusal, with its reason, and not by the deadline.
func requireEnded(t *testing.T, err error, reason string) {
	t.Helper()
	var refused *StartRefused
	if !errors.Is(err, ErrStartEnded) || !errors.As(err, &refused) || !refused.Ended || errors.Is(err, ErrDeadlinePassed) ||
		errors.Is(err, ErrEndUnproven) || refused.Reason != reason {
		t.Fatalf("refused start: %v, want it ended with reason %q", err, reason)
	}
}

// A first visit whose placement refuses it for good ends at once, with or
// without a deadline, as one its deadline overtook does (ADR 0067): the
// preparation is fenced, nothing is recorded as a visit, and the execution
// is idle with the checkpoint it started from, which a new execution
// continues. Recover, stop and a replay of the start then report it ended
// without asking the placement again.
func TestFirstVisitRefusedByItsPlacementEndsIdle(t *testing.T) {
	t.Parallel()
	for _, deadline := range []string{"deadline ahead", "no deadline"} {
		t.Run(deadline, func(t *testing.T) {
			t.Parallel()
			c, fencing, seeded := fencingSetup(t)
			first := seeded.Source
			first.ExecutionID, first.OperationID, first.AllocationID = uuid.NewString(), uuid.NewString(), uuid.NewString()
			first.Deadline = time.Now().Add(time.Hour).UTC()
			if deadline == "no deadline" {
				first.Deadline = time.Time{}
			}
			if _, err := c.Create(first, seeded.Input, ""); err != nil {
				t.Fatal(err)
			}
			fencing.refuse = &StartRefused{Reason: testRefusal}
			r, err := c.Recover(t.Context(), first.ExecutionID)
			requireEnded(t, err, testRefusal)
			if !strings.Contains(err.Error(), testRefusal) {
				t.Fatalf("the refusal lost its reason: %v", err)
			}
			checkAbandoned(t, c, r, nil, nil)
			if fencing.prepareCalls != 1 || fencing.abortPreparations != 1 || len(r.Visits) != 0 ||
				r.Checkpoint.Metadata.SHA256 != first.CheckpointDigest || r.Remaining[testCapability] != first.Allowances[testCapability] {
				t.Fatalf("refused start: prepared %d, fenced %d, visits %d", fencing.prepareCalls, fencing.abortPreparations, len(r.Visits))
			}
			if !r.Abandoned(first.OperationID) {
				t.Fatal("the refused start is not recorded as ended")
			}
			if r, err = c.Recover(t.Context(), first.ExecutionID); err != nil || r.Phase != PhaseIdle {
				t.Fatalf("recover of the ended start: %s %v", r.Phase, err)
			}
			if r, err = c.Finalize(t.Context(), first.ExecutionID, uuid.NewString()); err != nil || r.Phase != PhaseIdle {
				t.Fatalf("stop of the ended start: %s %v", r.Phase, err)
			}
			// The journal keeps no reason: a replay says only that the
			// placement refused it, never that the deadline passed.
			r, err = c.Move(t.Context(), first.ExecutionID, first.OperationID, testLocal)
			requireEnded(t, err, "")
			if err = c.StartEnded(r); !errors.Is(err, ErrStartRefused) || errors.Is(err, ErrDeadlinePassed) {
				t.Fatalf("ended start before its deadline: %v", err)
			}
			if fencing.prepareCalls != 1 {
				t.Fatalf("a replay prepared again: %d", fencing.prepareCalls)
			}
			fencing.refuse = nil
			resumeFrom(t, c, first.ExecutionID, first)
		})
	}
}

// A move whose destination refuses it ends after its source stopped: the
// execution is idle with the checkpoint that stop committed.
func TestMoveRefusedByItsDestinationKeepsTheStoppedCheckpoint(t *testing.T) {
	t.Parallel()
	c, _, _, remote, start := setup(t)
	id := start.Source.ExecutionID
	if r, err := c.Move(t.Context(), id, uuid.NewString(), testRemote); err != nil || r.Phase != PhaseActive {
		t.Fatalf("move to remote: %s %v", r.Phase, err)
	}
	c.placements[testLocal].(*fakePlacement).refuse = &StartRefused{Reason: testRefusal}
	r, err := c.Move(t.Context(), id, uuid.NewString(), testLocal)
	requireEnded(t, err, testRefusal)
	checkAbandoned(t, c, r, nil, nil)
	if last := r.Visits[len(r.Visits)-1]; remote.stopped != 1 || last.Launch.Placement != testRemote ||
		r.Checkpoint.Metadata.SHA256 != remote.digest {
		t.Fatalf("refused move: remote stops %d, last visit at %s", remote.stopped, last.Launch.Placement)
	}
}

// Until the placement fences the refused preparation, the start stays
// preparing and the error says both why it cannot start and why it is not
// ended yet; the next recover ends it.
func TestRefusedStartWaitsForProofThatNothingRuns(t *testing.T) {
	t.Parallel()
	c, fencing, seeded := fencingSetup(t)
	first := createResumed(t, c, seeded)
	fencing.refuse = &StartRefused{Reason: testRefusal}
	fencing.abortPreparationErr = errors.New("cluster unreachable")
	r, err := c.Recover(t.Context(), first.ExecutionID)
	if !errors.Is(err, ErrEndUnproven) || !errors.Is(err, ErrStartRefused) || errors.Is(err, ErrStartEnded) ||
		!strings.Contains(err.Error(), testRefusal) || r.Phase != PhasePreparing {
		t.Fatalf("unfenced refused start: %s %v", r.Phase, err)
	}
	if stored, err := c.Inspect(first.ExecutionID); err != nil || stored.Phase != PhasePreparing {
		t.Fatalf("unfenced refused start was recorded: %s %v", stored.Phase, err)
	}
	fencing.abortPreparationErr = nil
	r, err = c.Recover(t.Context(), first.ExecutionID)
	requireEnded(t, err, testRefusal)
	checkAbandoned(t, c, r, nil, nil)
}

// A refusal's reason is cut to reasonMax bytes on a rune boundary, so a
// controller message in any script stays valid UTF-8 within the bound, and
// a reason at the bound is kept whole.
func TestRefusalReasonIsCutOnARuneBoundary(t *testing.T) {
	t.Parallel()
	for _, prefix := range []string{"", "a", "aa"} {
		long := prefix + strings.Repeat("ü", reasonMax) // two bytes each
		got := RefusalReason(long)
		if !utf8.ValidString(got) || len(got) > reasonMax || !strings.HasSuffix(got, "…") ||
			len(got) < reasonMax-len("…")-utf8.UTFMax {
			t.Errorf("prefix %q: cut to %d bytes, valid %t: %q", prefix, len(got), utf8.ValidString(got), got[len(got)-8:])
		}
	}
	exact := strings.Repeat("a", reasonMax)
	if got := RefusalReason(exact); got != exact {
		t.Errorf("a reason at the bound was changed to %d bytes", len(got))
	}
}

// A stop that ends a pending start because its deadline passed, and finds
// the placement refusing it instead, as when the coordinator's clock steps
// back across the deadline mid-stop, still stops: the start ended either
// way, and the stop finalizes the idle execution. With a clock that never
// runs backwards a stop never prepares after the deadline, so this guards
// the error the stop accepts rather than a path seen in use.
func TestStopFinishesAStartEndedByARefusalAtTheDeadline(t *testing.T) {
	t.Parallel()
	c, fencing, seeded := fencingSetup(t)
	first := createResumed(t, c, seeded)
	fencing.refuse = &StartRefused{Reason: testRefusal}
	var calls atomic.Int32
	c.now = func() time.Time {
		if calls.Add(1) == 1 {
			return first.Deadline
		}
		return first.Deadline.Add(-time.Minute)
	}
	r, err := c.Finalize(t.Context(), first.ExecutionID, uuid.NewString())
	if err != nil || r.Phase != PhaseIdle || fencing.prepareCalls != 1 {
		t.Fatalf("stop of a start refused as its deadline passed: %s %v, prepared %d", r.Phase, err, fencing.prepareCalls)
	}
}

// Only a refusal ends the start: any other Prepare failure leaves it
// preparing for recover, as before.
func TestOtherPrepareFailuresDoNotEndTheStart(t *testing.T) {
	t.Parallel()
	c, fencing, seeded := fencingSetup(t)
	first := createResumed(t, c, seeded)
	fencing.refuse = errors.New("placement unreachable")
	r, err := c.Recover(t.Context(), first.ExecutionID)
	if err == nil || errors.Is(err, ErrStartEnded) || r.Phase != PhasePreparing || fencing.abortPreparations != 0 {
		t.Fatalf("failed prepare: %s %v, fenced %d", r.Phase, err, fencing.abortPreparations)
	}
}
