//nolint:goconst // Keep independently stated literal behavior expectations.
package work

import (
	"crypto/tls"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/google/uuid"
)

// assertRemoved checks that nothing of session v is kept: no record, no
// journal of any of its executions.
func assertRemoved(t *testing.T, c Config, o *Owner, id string, executions ...string) {
	t.Helper()
	if _, err := os.Lstat(o.path(id, ".json")); !os.IsNotExist(err) {
		t.Fatalf("record of %s kept: %v", id, err)
	}
	for _, execution := range executions {
		if _, err := c.Coordinator.Inspect(execution); !os.IsNotExist(err) {
			t.Fatalf("journal of execution %s kept: %v", execution, err)
		}
	}
	if _, err := o.Info(t.Context(), id); code(err) != profile.CodeInvalid {
		t.Fatalf("removed session still answers: %v", err)
	}
}

// remove refuses a session that is starting, running, stopping, has
// unfinished work or is being pulled, and keeps it.
func TestRemoveRefusesSessionsThatAreNotSafelyRemovable(t *testing.T) {
	t.Parallel()
	refuse := func(t *testing.T, o *Owner, id string, phase Phase) {
		t.Helper()
		if v, err := o.Info(t.Context(), id); err != nil || v.Phase != phase {
			t.Fatalf("set-up phase %s: %+v %v", phase, v, err)
		}
		if _, err := o.Remove(t.Context(), id, false); code(err) != profile.CodeConflict {
			t.Fatalf("removed a session that is %s: %v", phase, err)
		}
		if v, err := o.Info(t.Context(), id); err != nil || v.Phase != phase {
			t.Fatalf("a refused removal changed the session: %+v %v", v, err)
		}
	}
	t.Run("active", func(t *testing.T) {
		t.Parallel()
		c, _ := fixture(t)
		o := openOwner(t, c)
		refuse(t, o, startInteractive(t, o, interactiveRequest()).ID, phaseActive)
	})
	// A task whose turn is still running reads active, though the journal
	// holds its completion wait as stopping (completing).
	t.Run("running task", func(t *testing.T) {
		t.Parallel()
		c, p := fixture(t)
		release := make(chan struct{})
		p.heldStop, p.stopStarted = release, make(chan struct{})
		o := openOwner(t, c)
		defer close(release)
		v := submit(t, o)
		<-p.stopStarted
		refuse(t, o, v.ID, phaseActive)
	})
	// An interactive session whose owner's stop waits for the agent's turn
	// is stopping.
	t.Run("stopping", func(t *testing.T) {
		t.Parallel()
		c, p := fixture(t)
		o := openOwner(t, c)
		v := startInteractive(t, o, interactiveRequest())
		p.mu.Lock()
		p.turnPending = 1
		p.mu.Unlock()
		if waiting, err := o.Stop(t.Context(), v.ID, uuid.NewString()); err != nil || waiting.Phase != phaseStopping {
			t.Fatalf("stop waiting for the turn: %+v %v", waiting, err)
		}
		refuse(t, o, v.ID, phaseStopping)
	})
	t.Run("preparing", func(t *testing.T) {
		t.Parallel()
		c, p := fixture(t)
		p.refusePrepare = &profile.Rejection{Code: profile.CodeDenied}
		o := openOwner(t, c)
		v := submit(t, o)
		waitDriver(t, o, v.ID)
		refuse(t, o, v.ID, phasePreparing)
	})
	for name, phase := range map[string]Phase{"fence": phaseUnknownStop, "accounting": phaseUnknownUsage} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			c, p := fixture(t)
			p.missingFence = name == "fence"
			p.missingSeal = name == "accounting"
			o := openOwner(t, c)
			v := submit(t, o)
			waitPhase(t, o, v.ID, phase)
			refuse(t, o, v.ID, phase)
		})
	}
	t.Run("exporting", func(t *testing.T) {
		t.Parallel()
		c, _ := fixture(t)
		o := openOwner(t, c)
		v := submit(t, o)
		waitPhase(t, o, v.ID, phaseRetained)
		if _, err := o.BeginExport(t.Context(), v.ID, uuid.NewString()); err != nil {
			t.Fatal(err)
		}
		refuse(t, o, v.ID, PhaseExporting)
	})
}

// Removing a session frees what it kept, so a submission the executor
// refused for space then starts. A repeated removal reports it removed, and
// a repeated submission of the removed session's request never starts it
// again.
func TestRemoveFreesCapacityAndIsReplaySafe(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	c.RetainedBytes = 2 * c.MaxCheckpointBytes
	o := openOwner(t, c)
	first := taskRequest()
	v, err := o.Submit(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, o, v.ID, phaseRetained)
	refused := taskRequest()
	if _, err = o.Submit(t.Context(), refused); code(err) != profile.CodeRetentionFull {
		t.Fatalf("submission beyond retainedBytes: %v", err)
	}

	removed, err := o.Remove(t.Context(), v.ID, false)
	if err != nil || removed.ID != v.ID {
		t.Fatalf("remove a retained task: %+v %v", removed, err)
	}
	assertRemoved(t, c, o, v.ID, v.ExecutionID)
	if again, err := o.Remove(t.Context(), v.ID, false); err != nil || again.ID != v.ID {
		t.Fatalf("repeated removal: %+v %v", again, err)
	}
	if _, err = o.Remove(t.Context(), uuid.NewString(), false); code(err) != profile.CodeInvalid {
		t.Fatalf("removed a session that never existed: %v", err)
	}
	if _, err = o.Submit(t.Context(), first); code(err) != profile.CodeConflict {
		t.Fatalf("a repeated submission restarted a removed session: %v", err)
	}
	next, err := o.Submit(t.Context(), refused)
	if err != nil {
		t.Fatalf("submission after removal: %v", err)
	}
	waitPhase(t, o, next.ID, phaseRetained)

	// A removal interrupted after the record went finishes when repeated.
	if err = o.markRemoved(removedMark{ID: next.ID, Executions: []string{next.ExecutionID}}); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(o.path(next.ID, ".json")); err != nil {
		t.Fatal(err)
	}
	if _, err = o.Remove(t.Context(), next.ID, false); err != nil {
		t.Fatalf("repeat an interrupted removal: %v", err)
	}
	assertRemoved(t, c, o, next.ID, next.ExecutionID)
}

// A settled session is removed promptly while another session's stop waits
// for its agent's turn, and the room it frees admits a submission the
// executor refused as full.
func TestRemoveWhileAnotherSessionsStopWaits(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	o := openOwner(t, c)
	done := submit(t, o)
	waitPhase(t, o, done.ID, phaseRetained)
	release := make(chan struct{})
	p.mu.Lock()
	p.heldStop, p.stopStarted = release, make(chan struct{})
	started := p.stopStarted
	p.mu.Unlock()
	defer close(release)
	running := submit(t, o)
	<-started

	// Room for everything but the last byte of the running session's and the
	// new submission's reservations: removing anything makes room.
	o.config.RetainedBytes = keptBytes(t, c) + 3*c.MaxCheckpointBytes - 1
	refused := taskRequest()
	if _, err := o.Submit(t.Context(), refused); code(err) != profile.CodeRetentionFull {
		t.Fatalf("submission to a full executor: %v", err)
	}
	began := time.Now()
	if _, err := o.Remove(t.Context(), done.ID, false); err != nil {
		t.Fatalf("remove while another session stops: %v", err)
	}
	if waited := time.Since(began); waited > 2*time.Second {
		t.Fatalf("removal waited %s for another session's stop", waited)
	}
	assertRemoved(t, c, o, done.ID, done.ExecutionID)
	if _, err := o.Submit(t.Context(), refused); err != nil {
		t.Fatalf("submission after removal while another session stops: %v", err)
	}
	// The task's turn still runs: its completion wait reads active.
	if v, err := o.Info(t.Context(), running.ID); err != nil || v.Phase != phaseActive {
		t.Fatalf("the waiting stop was disturbed: %+v %v", v, err)
	}
}

// A session another, unstarted session starts from is kept until that one
// has started or is cancelled: its start reads the first's checkpoint.
func TestRemoveRefusesTheSourceOfAnUnstartedSession(t *testing.T) {
	t.Parallel()
	c, _ := projectFixture(t)
	o := openOwner(t, c)
	v, err := o.Submit(t.Context(), projectRequest("sproozi-dev"))
	if err != nil {
		t.Fatal(err)
	}
	source := waitPhase(t, o, v.ID, phaseRetained)
	// No driver starts what is submitted from here on.
	o.stopAll()
	q := projectRequest("sproozi-dev")
	q.WorkspaceFrom = &RevisionSource{SessionID: source.ID, Digest: source.CheckpointDigest,
		Revision: source.CheckpointInfo.Workspace.Revision}
	fresh, err := o.Submit(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = o.Remove(t.Context(), source.ID, false); code(err) != profile.CodeConflict {
		t.Fatalf("removed the source of an unstarted session: %v", err)
	}
	if _, err = o.Info(t.Context(), source.ID); err != nil {
		t.Fatalf("a refused removal removed the source: %v", err)
	}
	if _, err = o.Cancel(t.Context(), fresh.ID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err = o.Remove(t.Context(), source.ID, false); err != nil {
		t.Fatalf("remove the source of a cancelled session: %v", err)
	}
	// A submission naming a removed session as its start is refused.
	q.RequestID = uuid.NewString()
	if _, err = o.Submit(t.Context(), q); code(err) != profile.CodeInvalid {
		t.Fatalf("submission from a removed session: %v", err)
	}
}

// A record that cannot be read might start from the session being removed,
// so removal is refused rather than strand it.
func TestRemoveRefusesWhileAnotherRecordCannotBeRead(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	o := openOwner(t, c)
	v := submit(t, o)
	waitPhase(t, o, v.ID, phaseRetained)
	unreadable := o.path(uuid.NewString(), ".json")
	if err := os.WriteFile(unreadable, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Remove(t.Context(), v.ID, false); code(err) != profile.CodeConflict {
		t.Fatalf("removed while another record could not be read: %v", err)
	}
	if err := os.Remove(unreadable); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Remove(t.Context(), v.ID, false); err != nil {
		t.Fatal(err)
	}
}

// A removal and a submission that starts from the removed session race:
// exactly one wins, and the loser changes nothing.
func TestRemoveRacesASubmissionFromIt(t *testing.T) {
	t.Parallel()
	c, _ := projectFixture(t)
	o := openOwner(t, c)
	sources := make([]View, 0, 4)
	for range 4 {
		v, err := o.Submit(t.Context(), projectRequest("sproozi-dev"))
		if err != nil {
			t.Fatal(err)
		}
		sources = append(sources, v)
	}
	for i, v := range sources {
		sources[i] = waitPhase(t, o, v.ID, phaseRetained)
	}
	// No driver starts the dependants, so a winning one stays unstarted.
	o.stopAll()
	for _, source := range sources {
		q := projectRequest("sproozi-dev")
		q.WorkspaceFrom = &RevisionSource{SessionID: source.ID, Digest: source.CheckpointDigest,
			Revision: source.CheckpointInfo.Workspace.Revision}
		var removeErr, submitErr error
		var dependant View
		var wg sync.WaitGroup
		wg.Go(func() { _, removeErr = o.Remove(t.Context(), source.ID, false) })
		wg.Go(func() { dependant, submitErr = o.Submit(t.Context(), q) })
		wg.Wait()
		switch {
		case removeErr == nil && submitErr != nil:
			if code(submitErr) != profile.CodeInvalid {
				t.Fatalf("submission after the removal: %v", submitErr)
			}
		case removeErr != nil && submitErr == nil:
			if code(removeErr) != profile.CodeConflict {
				t.Fatalf("removal after the submission: %v", removeErr)
			}
			if _, err := o.Info(t.Context(), source.ID); err != nil {
				t.Fatalf("the dependant's source went: %v", err)
			}
			if _, err := o.Info(t.Context(), dependant.ID); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("removal %v and submission %v both won or both lost", removeErr, submitErr)
		}
	}
}

// A submission whose source session is removed while the submission reads
// it, at any step before the submission is retained, is answered as one
// whose source was removed first: invalid, with the same message, never a
// filesystem error or conflict, and nothing of it is kept. The removal runs
// to completion inside Config.interleave after the named step, so the
// interleaving does not depend on timing:
//
//   - record: after the source's catalogue record, before Coordinator.Inspect,
//     which then finds no journal (was conflict);
//   - execution: after Coordinator.Inspect, before stoppedVisit's
//     Coordinator.Checkpoint, which then fails to open the journal (was a raw
//     filesystem error);
//   - checkpoint: after the whole read, before Submit takes o.retention; the
//     check under that lock answers it.
func TestSubmissionFromASourceRemovedWhileItIsRead(t *testing.T) {
	t.Parallel()
	for _, step := range []string{stepSourceRecord, stepSourceExecution, stepSourceCheckpoint} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			c, _ := projectFixture(t)
			var o *Owner
			var source View
			var removed bool
			var removeErr error
			// The source steps run only in revisionSource, in this test's
			// goroutine: sources start from nothing, and no driver runs a
			// dependant. Every other step returns before reading anything.
			c.interleave = func(at, _ string) {
				if at != step || removed {
					return
				}
				removed = true
				_, removeErr = o.Remove(t.Context(), source.ID, false)
			}
			o = openOwner(t, c)
			v, err := o.Submit(t.Context(), projectRequest("sproozi-dev"))
			if err != nil {
				t.Fatal(err)
			}
			source = waitPhase(t, o, v.ID, phaseRetained)
			o.stopAll()
			q := projectRequest("sproozi-dev")
			q.WorkspaceFrom = &RevisionSource{SessionID: source.ID, Digest: source.CheckpointDigest,
				Revision: source.CheckpointInfo.Workspace.Revision}
			_, submitErr := o.Submit(t.Context(), q)
			if !removed || removeErr != nil {
				t.Fatalf("removal at the %s step: ran %t, %v", step, removed, removeErr)
			}
			if code(submitErr) != profile.CodeInvalid {
				t.Fatalf("submission from a source removed after its %s step: %v", step, submitErr)
			}
			assertRemoved(t, c, o, source.ID, source.ExecutionID)
			if id := o.sessionID(q.RequestID); o.recorded(id) || o.wasRemoved(id) {
				t.Fatalf("a refused submission kept session %s", id)
			}
			q.RequestID = uuid.NewString()
			if _, after := o.Submit(t.Context(), q); after == nil || after.Error() != submitErr.Error() {
				t.Fatalf("submission after the removal: %v, during it: %v", after, submitErr)
			}
		})
	}
}

// A removal Open cannot finish is logged, not fatal: the executor starts,
// the session is neither listed nor started again, and a later remove
// retries it.
func TestOpenStartsWhenARemovalCannotFinish(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	v := submit(t, o)
	waitPhase(t, o, v.ID, phaseRetained)
	if err = o.markRemoved(removedMark{ID: v.ID, Executions: []string{v.ExecutionID}}); err != nil {
		t.Fatal(err)
	}
	if err = o.Close(); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(filepath.Dir(c.Directory), "sessions", v.ExecutionID+".json")
	good, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(journal, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	started := p.initial
	p.mu.Unlock()
	o = openOwner(t, c)
	if _, err = o.Info(t.Context(), v.ID); code(err) != profile.CodeInvalid {
		t.Fatalf("a session being removed answers: %v", err)
	}
	if listed, err := o.List(t.Context(), SessionQuery{}); err != nil || len(listed) != 0 {
		t.Fatalf("a session being removed is listed: %+v %v", listed, err)
	}
	if _, err = o.Recover(t.Context(), v.ID); code(err) != profile.CodeInvalid {
		t.Fatalf("a session being removed recovers: %v", err)
	}
	if _, err = o.Remove(t.Context(), v.ID, false); err == nil {
		t.Fatal("finished a removal whose journal cannot be read")
	}
	if err = os.WriteFile(journal, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = o.Remove(t.Context(), v.ID, false); err != nil {
		t.Fatalf("a later remove did not finish the removal: %v", err)
	}
	assertRemoved(t, c, o, v.ID, v.ExecutionID)
	time.Sleep(50 * time.Millisecond)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.initial != started {
		t.Fatal("a session being removed was started again")
	}
}

// A finished removal leaves only a small mark, with no executions.
func TestRemoveLeavesASmallMark(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	o := openOwner(t, c)
	v := submit(t, o)
	waitPhase(t, o, v.ID, phaseRetained)
	if _, err := o.Remove(t.Context(), v.ID, false); err != nil {
		t.Fatal(err)
	}
	mark, marked, err := o.readRemoved(v.ID)
	if err != nil || !marked || len(mark.Executions) != 0 {
		t.Fatalf("mark after a finished removal: %+v %v %v", mark, marked, err)
	}
	if info, err := os.Stat(o.path(v.ID, removedSuffix)); err != nil || info.Size() > 128 {
		t.Fatalf("mark size: %v %v", info, err)
	}
}

// The executor finishes a removal a crash interrupted before it starts any
// session, whichever of the record and the journals are left: it never
// starts the removed session again, and nothing it kept stays.
func TestOpenFinishesInterruptedRemovals(t *testing.T) {
	t.Parallel()
	for _, left := range []string{"record and journals", "journals", "record"} {
		t.Run(left, func(t *testing.T) {
			t.Parallel()
			c, p := fixture(t)
			o, err := Open(c)
			if err != nil {
				t.Fatal(err)
			}
			v := submit(t, o)
			waitPhase(t, o, v.ID, phaseRetained)
			if err = o.markRemoved(removedMark{ID: v.ID, Executions: []string{v.ExecutionID}}); err != nil {
				t.Fatal(err)
			}
			if err = o.Close(); err != nil {
				t.Fatal(err)
			}
			journal := filepath.Join(filepath.Dir(c.Directory), "sessions", v.ExecutionID+".json")
			if !strings.Contains(left, "record") {
				if err = os.Remove(o.path(v.ID, ".json")); err != nil {
					t.Fatal(err)
				}
			}
			if !strings.Contains(left, "journals") {
				if err = os.Remove(journal); err != nil {
					t.Fatal(err)
				}
			}
			p.mu.Lock()
			started := p.initial
			p.mu.Unlock()
			o = openOwner(t, c)
			if _, err = os.Lstat(journal); !os.IsNotExist(err) {
				t.Fatalf("journal kept after reopening: %v", err)
			}
			assertRemoved(t, c, o, v.ID, v.ExecutionID)
			if mark, marked, err := o.readRemoved(v.ID); err != nil || !marked || len(mark.Executions) != 0 {
				t.Fatalf("mark after reopening: %+v %v %v", mark, marked, err)
			}
			if listed, err := o.List(t.Context(), SessionQuery{}); err != nil || len(listed) != 0 {
				t.Fatalf("listed after reopening: %+v %v", listed, err)
			}
			time.Sleep(50 * time.Millisecond)
			p.mu.Lock()
			defer p.mu.Unlock()
			if p.initial != started {
				t.Fatal("reopening started a removed session again")
			}
		})
	}
}

// Every execution of an interactive session goes, and a cancelled session
// and a pulled one are removable.
func TestRemoveDeletesEveryExecutionAndAcceptsCancelledAndPulled(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	o := openOwner(t, c)

	v := startInteractive(t, o, interactiveRequest())
	if _, err := o.Stop(t.Context(), v.ID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	resumed, err := o.Resume(t.Context(), v.ID, uuid.NewString(), "")
	if err != nil {
		t.Fatal(err)
	}
	waitPhase(t, o, v.ID, phaseActive)
	if _, err = o.Stop(t.Context(), v.ID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err = o.Remove(t.Context(), v.ID, false); err != nil {
		t.Fatal(err)
	}
	assertRemoved(t, c, o, v.ID, v.ExecutionID, resumed.ExecutionID)

	cancelled := startInteractive(t, o, interactiveRequest())
	if _, err = o.Cancel(t.Context(), cancelled.ID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, o, cancelled.ID, phaseCancelled)
	if _, err = o.Remove(t.Context(), cancelled.ID, false); err != nil {
		t.Fatal(err)
	}
	assertRemoved(t, c, o, cancelled.ID, cancelled.ExecutionID)

	pulled := submit(t, o)
	waitPhase(t, o, pulled.ID, phaseRetained)
	op := uuid.NewString()
	if _, err = o.BeginExport(t.Context(), pulled.ID, op); err != nil {
		t.Fatal(err)
	}
	if _, err = o.Claim(t.Context(), pulled.ID, op, p.meta.SHA256); err != nil {
		t.Fatal(err)
	}
	// A pulled session goes only for a caller whose machine holds the pull
	// as finished: the executor no longer answers a repeated claim.
	if _, err = o.Remove(t.Context(), pulled.ID, false); code(err) != profile.CodeConflict {
		t.Fatalf("removed a pulled session for a caller that does not hold the pull: %v", err)
	}
	if _, err = o.Remove(t.Context(), pulled.ID, true); err != nil {
		t.Fatal(err)
	}
	assertRemoved(t, c, o, pulled.ID, pulled.ExecutionID)
	if _, err = o.Claim(t.Context(), pulled.ID, op, p.meta.SHA256); err == nil {
		t.Fatal("a removed session was claimed again")
	}
	if kept, err := os.ReadDir(c.Directory); err != nil {
		t.Fatal(err)
	} else {
		for _, entry := range kept {
			if strings.HasSuffix(entry.Name(), ".json") || strings.HasSuffix(entry.Name(), ".workspace") {
				t.Fatalf("catalogue keeps %s after every session was removed", entry.Name())
			}
		}
	}
}

// A stopped version 1 record is removable with its kept checkpoint; one
// being pulled is not.
func TestRemoveVersionOneRecords(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	o := openOwner(t, c)
	v := submit(t, o)
	waitPhase(t, o, v.ID, phaseRetained)
	export, err := o.BeginExport(t.Context(), v.ID, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	exporting := writeLegacy(t, c, o, legacyExporting, export)
	if _, err = o.Remove(t.Context(), exporting, false); code(err) != profile.CodeConflict {
		t.Fatalf("removed a version 1 record being pulled: %v", err)
	}
	for _, phase := range []legacyPhase{legacyRetained, legacyCancelled, legacyPulled} {
		id := writeLegacy(t, c, o, phase, export)
		if _, err = o.Remove(t.Context(), id, phase == legacyPulled); err != nil {
			t.Fatalf("remove a %s version 1 record: %v", phase, err)
		}
		assertRemoved(t, c, o, id)
		if _, err = os.Lstat(o.path(id, ".checkpoint")); !os.IsNotExist(err) {
			t.Fatalf("kept the checkpoint of a removed %s version 1 record: %v", phase, err)
		}
	}
}

// Only the home principal removes a session; the work token is denied, as
// for every owner action Hermes does not hold.
func TestWorkTokenCannotRemove(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	o := openOwner(t, c)
	v := submit(t, o)
	waitPhase(t, o, v.ID, phaseRetained)
	taskToken, home := strings.Repeat("t", 32), strings.Repeat("h", 32)
	h, err := NewHandler(o, taskToken, homeToken(home))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		token string
		want  int
	}{{taskToken, http.StatusForbidden}, {home, http.StatusOK}, {home, http.StatusOK}} {
		b, _ := json.Marshal(map[string]string{"id": v.ID})
		r := httptest.NewRequest(http.MethodPost, "/work/remove", strings.NewReader(string(b)))
		r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
		r.Header.Set("Authorization", "Bearer "+tc.token)
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("remove with token %q…: %d %s, want %d", tc.token[:1], w.Code, w.Body.String(), tc.want)
		}
		if tc.want == http.StatusForbidden {
			if _, err = o.Info(t.Context(), v.ID); err != nil {
				t.Fatalf("a denied removal removed the session: %v", err)
			}
		}
	}
	assertRemoved(t, c, o, v.ID, v.ExecutionID)
}
