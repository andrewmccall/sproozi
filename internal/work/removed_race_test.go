//nolint:goconst // Keep independently stated literal behavior expectations.
package work

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/google/uuid"
)

// interleaving runs a removal once, the first time Config.interleave
// reaches step for session id. Every command and driver may reach a step, so
// it is guarded.
type interleaving struct {
	mu       sync.Mutex
	step, id string
	run      func()
	ran      bool
}

func (i *interleaving) at(step, id string) {
	i.mu.Lock()
	run := i.run
	if run == nil || step != i.step || id != i.id {
		i.mu.Unlock()
		return
	}
	i.run, i.ran = nil, true
	i.mu.Unlock()
	run()
}

func (i *interleaving) arm(step, id string, run func()) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.step, i.id, i.run, i.ran = step, id, run, false
}

func (i *interleaving) fired() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.ran
}

// The commands of the removal table that also run on a resumed session.
const (
	commandInfo    = "info"
	commandList    = "list"
	commandRecover = "recover"
	commandAttach  = "attach"
)

// removedSession is a settled session a race test removes.
type removedSession struct {
	View
	request Submit
	pull    string
	digest  string
}

// errText is err's message, or empty for no error.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Open still deletes the upload of a removed session, which read now
// answers as invalid rather than as a missing record.
func TestOpenRemovesTheUploadOfARemovedSession(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.NewString()
	if err = o.markRemoved(removedMark{ID: id}); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(o.path(id, uploadSuffix), []byte("upload"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = o.Close(); err != nil {
		t.Fatal(err)
	}
	o = openOwner(t, c)
	if _, err = os.Lstat(o.path(id, uploadSuffix)); !os.IsNotExist(err) {
		t.Fatalf("kept the upload of a removed session: %v", err)
	}
}

// An owner command on a session removed while the command runs answers
// exactly as the same command on a session removed before it started: the
// same code and message, never a filesystem error (unavailable), conflict,
// or a phase such as preparing for a session that is gone. The removal is a
// real Remove, run to completion inside Config.interleave at the named step
// of the command's own goroutine, so nothing depends on timing:
//
//   - entry: after the command read the session's record without a lock.
//     Pull, claim, cancel, resume and stop then wait for the jobs lock Remove
//     held and read the record again (was a raw read error); info, list,
//     recover, a repeated submission and attach then ask the coordinator for
//     the execution, which is gone (was preparing, or conflict for attach).
//   - stop-bound: after a stop saved its binding and released the lock,
//     before Coordinator.Finalize (was a raw error opening the journal).
//
// The command is then run again, on the removed session, and must answer
// the same.
func TestCommandsOnASessionRemovedWhileTheyRun(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	cases := []struct {
		name        string
		interactive bool
		resumed     bool
		pulled      bool
		step        string
		want        profile.Code
		run         func(o *Owner, s removedSession) error
	}{
		{name: commandInfo, step: stepEntry, want: profile.CodeInvalid, run: func(o *Owner, s removedSession) error {
			_, err := o.Info(ctx, s.ID)
			return err
		}},
		{name: commandList, step: stepEntry, run: func(o *Owner, s removedSession) error {
			listed, err := o.List(ctx, SessionQuery{})
			for _, v := range listed {
				if v.ID == s.ID {
					return errors.New("listed as " + string(v.Phase))
				}
			}
			return err
		}},
		{name: commandRecover, step: stepEntry, want: profile.CodeInvalid, run: func(o *Owner, s removedSession) error {
			_, err := o.Recover(ctx, s.ID)
			return err
		}},
		{name: "replay", step: stepEntry, want: profile.CodeConflict, run: func(o *Owner, s removedSession) error {
			_, err := o.Submit(ctx, s.request)
			return err
		}},
		{name: "pull", step: stepEntry, want: profile.CodeInvalid, run: func(o *Owner, s removedSession) error {
			_, err := o.BeginExport(ctx, s.ID, uuid.NewString())
			return err
		}},
		{name: "repeated claim", pulled: true, step: stepEntry, want: profile.CodeInvalid, run: func(o *Owner, s removedSession) error {
			_, err := o.Claim(ctx, s.ID, s.pull, s.digest)
			return err
		}},
		{name: "cancel", step: stepEntry, want: profile.CodeInvalid, run: func(o *Owner, s removedSession) error {
			_, err := o.Cancel(ctx, s.ID, uuid.NewString())
			return err
		}},
		{name: "resume", interactive: true, step: stepEntry, want: profile.CodeInvalid, run: func(o *Owner, s removedSession) error {
			_, err := o.Resume(ctx, s.ID, uuid.NewString(), "")
			return err
		}},
		{name: "stop", interactive: true, step: stepEntry, want: profile.CodeInvalid, run: func(o *Owner, s removedSession) error {
			_, err := o.Stop(ctx, s.ID, uuid.NewString())
			return err
		}},
		{name: "stop bound", interactive: true, step: stepStopBound, want: profile.CodeInvalid, run: func(o *Owner, s removedSession) error {
			_, err := o.Stop(ctx, s.ID, uuid.NewString())
			return err
		}},
		{name: commandAttach, interactive: true, step: stepEntry, want: profile.CodeInvalid, run: func(o *Owner, s removedSession) error {
			_, err := o.Attach(ctx, s.ID)
			return err
		}},
	}
	// A resumed interactive session's current execution is not the session:
	// a removal is recognized by the session's mark, not the execution's.
	for _, tc := range cases {
		switch tc.name {
		case commandInfo, commandList, commandRecover, commandAttach:
			tc.name, tc.interactive, tc.resumed = tc.name+" resumed", true, true
			cases = append(cases, tc)
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c, p := fixture(t)
			race := &interleaving{}
			c.interleave = race.at
			o := openOwner(t, c)
			var s removedSession
			if tc.interactive {
				s.request = interactiveRequest()
				v := startInteractive(t, o, s.request)
				if _, err := o.Stop(t.Context(), v.ID, uuid.NewString()); err != nil {
					t.Fatal(err)
				}
				s.View = waitPhase(t, o, v.ID, phaseRetained)
				if tc.resumed {
					s.View = resumeAndStop(t, o, s.ID)
				}
			} else {
				s.request = taskRequest()
				v, err := submitRetained(t, o, CallerHome, s.request)
				if err != nil {
					t.Fatal(err)
				}
				s.View = waitPhase(t, o, v.ID, phaseRetained)
			}
			if tc.pulled {
				s.pull, s.digest = uuid.NewString(), p.meta.SHA256
				if _, err := o.BeginExport(t.Context(), s.ID, s.pull); err != nil {
					t.Fatal(err)
				}
				if _, err := o.Claim(t.Context(), s.ID, s.pull, s.digest); err != nil {
					t.Fatal(err)
				}
			}
			var removeErr error
			race.arm(tc.step, s.ID, func() { _, removeErr = o.Remove(t.Context(), s.ID, tc.pulled) })
			during := tc.run(o, s)
			if !race.fired() || removeErr != nil {
				t.Fatalf("removal at the %s step: ran %t, %v", tc.step, race.fired(), removeErr)
			}
			assertRemoved(t, c, o, s.ID, s.ID, s.ExecutionID)
			after := tc.run(o, s)
			if code(during) != tc.want || errText(during) != errText(after) {
				t.Fatalf("%s on a session removed at its %s step: %v (code %q); on a removed session: %v",
					tc.name, tc.step, during, code(during), after)
			}
		})
	}
}

// resumeAndStop resumes a stopped interactive session and stops it again, so
// its current execution is no longer the session's own ID.
func resumeAndStop(t *testing.T, o *Owner, id string) View {
	t.Helper()
	if _, err := o.Resume(t.Context(), id, uuid.NewString(), ""); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, o, id, phaseActive)
	if _, err := o.Stop(t.Context(), id, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	v := waitPhase(t, o, id, phaseRetained)
	if v.ExecutionID == id {
		t.Fatalf("resume kept the session's execution: %+v", v)
	}
	return v
}

// Pull and claim of a version 1 record read the record again under the jobs
// lock; one removed after their first read answers as removed (was a raw
// "no such file or directory", unavailable).
func TestLegacyPullOfARecordRemovedWhileItRuns(t *testing.T) {
	t.Parallel()
	for _, claim := range []bool{false, true} {
		t.Run(map[bool]string{false: "pull", true: "repeated claim"}[claim], func(t *testing.T) {
			t.Parallel()
			c, _ := fixture(t)
			race := &interleaving{}
			c.interleave = race.at
			o := openOwner(t, c)
			v := waitPhase(t, o, submit(t, o).ID, phaseRetained)
			export, err := o.BeginExport(t.Context(), v.ID, uuid.NewString())
			if err != nil {
				t.Fatal(err)
			}
			phase := legacyRetained
			if claim {
				phase = legacyPulled
			}
			legacy := writeLegacy(t, c, o, phase, export)
			op := uuid.NewString()
			run := func() error {
				if claim {
					_, err := o.Claim(t.Context(), legacy, op, export.Stopped.CheckpointDigest)
					return err
				}
				_, err := o.BeginExport(t.Context(), legacy, op)
				return err
			}
			var removeErr error
			race.arm(stepEntry, legacy, func() { _, removeErr = o.Remove(t.Context(), legacy, claim) })
			during := run()
			if !race.fired() || removeErr != nil {
				t.Fatalf("removal: ran %t, %v", race.fired(), removeErr)
			}
			after := run()
			if code(during) != profile.CodeInvalid || errText(during) != errText(after) {
				t.Fatalf("version 1 pull of a record removed after its entry: %v; of a removed record: %v", during, after)
			}
		})
	}
}

// An interactive recover reads the execution and then asks the coordinator
// to recover it. A cancellation and a removal in between leave no journal;
// the recover answers as for a removed session (was a raw "no such file or
// directory", unavailable). A resumed session's current execution is not
// the session: the removal is recognized by the session's mark.
func TestInteractiveRecoverOfASessionRemovedBeforeTheCoordinatorRecovers(t *testing.T) {
	t.Parallel()
	for _, resumed := range []bool{false, true} {
		t.Run(map[bool]string{false: "first execution", true: "resumed"}[resumed], func(t *testing.T) {
			t.Parallel()
			c, p := fixture(t)
			race := &interleaving{}
			c.interleave = race.at
			o := openOwner(t, c)
			v := startInteractive(t, o, interactiveRequest())
			if resumed {
				if _, err := o.Stop(t.Context(), v.ID, uuid.NewString()); err != nil {
					t.Fatal(err)
				}
				waitPhase(t, o, v.ID, phaseRetained)
				if _, err := o.Resume(t.Context(), v.ID, uuid.NewString(), ""); err != nil {
					t.Fatal(err)
				}
				if v = waitPhase(t, o, v.ID, phaseActive); v.ExecutionID == v.ID {
					t.Fatalf("resume kept the session's execution: %+v", v)
				}
			}
			loseStop(t, o, p, v.ID)
			var cancelErr, removeErr error
			race.arm(stepRecoverExecution, v.ID, func() {
				_, cancelErr = o.Cancel(t.Context(), v.ID, uuid.NewString())
				_, removeErr = o.Remove(t.Context(), v.ID, false)
			})
			_, during := o.Recover(t.Context(), v.ID)
			if !race.fired() || cancelErr != nil || removeErr != nil {
				t.Fatalf("removal: ran %t, cancel %v, remove %v", race.fired(), cancelErr, removeErr)
			}
			_, after := o.Recover(t.Context(), v.ID)
			if code(during) != profile.CodeInvalid || errText(during) != errText(after) {
				t.Fatalf("recover of a session removed before the coordinator recovered it: %v; of a removed session: %v",
					during, after)
			}
		})
	}
}

// loseStop stops active interactive session id with the placement's reply
// lost, leaving it unknown-stop.
func loseStop(t *testing.T, o *Owner, p *fixturePlacement, id string) {
	t.Helper()
	p.mu.Lock()
	p.lostStop = true
	p.mu.Unlock()
	if _, err := o.Stop(t.Context(), id, uuid.NewString()); err == nil {
		t.Fatal("a stop whose reply was lost succeeded")
	}
	waitPhase(t, o, id, phaseUnknownStop)
}

// An interactive recover whose Coordinator.Recover fails for a session that
// is not removed keeps that failure: only a removal is answered as one.
func TestRecoverKeepsAFailureOfAKeptSession(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	o := openOwner(t, c)
	v := startInteractive(t, o, interactiveRequest())
	loseStop(t, o, p, v.ID)
	p.mu.Lock()
	p.lostStop = true
	p.mu.Unlock()
	if _, err := o.Recover(t.Context(), v.ID); code(err) != profile.CodeUnavailable ||
		!strings.Contains(errText(err), "lost stopped reply") {
		t.Fatalf("recover whose stop reply was lost again: %v (code %q)", err, code(err))
	}
}

// The exit sweep says nothing about a session removed between its read of
// the record and its read of the execution: nothing exited. The test calls
// settleExit itself; the background sweep waits an hour, so it never reaches
// the step, and the removal's result is handed over a channel.
func TestExitSweepSkipsASessionRemovedAfterItsRead(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	c.exitInterval = time.Hour
	race := &interleaving{}
	c.interleave = race.at
	o := openOwner(t, c)
	v := startInteractive(t, o, interactiveRequest())
	if _, err := o.Stop(t.Context(), v.ID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, o, v.ID, phaseRetained)
	removal := make(chan error, 1)
	race.arm(stepExitRead, v.ID, func() {
		_, err := o.Remove(t.Context(), v.ID, false)
		removal <- err
	})
	stopped, err := o.settleExit(t.Context(), v.ID)
	if !race.fired() {
		t.Fatal("the removal did not run at the exit sweep's read")
	}
	if removeErr := <-removal; removeErr != nil {
		t.Fatalf("removal at the exit sweep's read: %v", removeErr)
	}
	if stopped || err != nil {
		t.Fatalf("exit sweep of a session removed after its read: stopped %t, %v", stopped, err)
	}
}

// The exit sweep still reports a failure to read the execution of a session
// that is not removed: only a removal is passed over.
func TestExitSweepReportsAnUnreadableExecution(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	c.exitInterval = time.Hour
	o := openOwner(t, c)
	v := startInteractive(t, o, interactiveRequest())
	if _, err := o.Stop(t.Context(), v.ID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	waitPhase(t, o, v.ID, phaseRetained)
	journal := filepath.Join(filepath.Dir(c.Directory), "sessions", v.ExecutionID+".json")
	if err := os.WriteFile(journal, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if stopped, err := o.settleExit(t.Context(), v.ID); stopped || err == nil {
		t.Fatalf("exit sweep of a session whose journal cannot be read: stopped %t, %v", stopped, err)
	}
}

// A stop whose Finalize fails for a session that is not removed keeps that
// failure: only a removal is answered as one.
func TestStopKeepsAFinalizeFailureOfAKeptSession(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	o := openOwner(t, c)
	v := startInteractive(t, o, interactiveRequest())
	p.mu.Lock()
	p.lostStop = true
	p.mu.Unlock()
	if _, err := o.Stop(t.Context(), v.ID, uuid.NewString()); code(err) != profile.CodeUnavailable {
		t.Fatalf("stop whose reply was lost: %v (code %q)", err, code(err))
	}
}

// List reports a record it cannot read, of a session that is not removed,
// rather than leaving it out.
func TestListReportsAnUnreadableRecord(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	o := openOwner(t, c)
	waitPhase(t, o, submit(t, o).ID, phaseRetained)
	if err := os.WriteFile(o.path(uuid.NewString(), ".json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if listed, err := o.List(t.Context(), SessionQuery{}); err == nil {
		t.Fatalf("listed %d sessions though a record cannot be read", len(listed))
	}
}

// A removal mark that cannot be checked is not taken as a removal: a command
// that relies on the mark to refuse, as a submission does, refuses with the
// underlying error, and one that uses it to explain a failure keeps the
// failure. With the catalogue's directory unsearchable, Info, List and a new
// submission each report the permission error (unavailable), never invalid,
// conflict or an empty list.
func TestAnUncheckableRemovalMarkIsReported(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	o := openOwner(t, c)
	v := waitPhase(t, o, submit(t, o).ID, phaseRetained)
	if err := os.Chmod(c.Directory, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(c.Directory, 0o700) })
	if _, err := os.Lstat(o.path(v.ID, removedSuffix)); !errors.Is(err, fs.ErrPermission) {
		t.Skipf("the directory's mode does not deny the check here: %v", err)
	}
	reported := func(what string, err error) {
		t.Helper()
		if code(err) != profile.CodeUnavailable || !errors.Is(err, fs.ErrPermission) {
			t.Errorf("%s with an uncheckable mark: %v (code %q)", what, err, code(err))
		}
	}
	_, err := o.Info(t.Context(), v.ID)
	reported("info command", err)
	listed, err := o.List(t.Context(), SessionQuery{})
	if err == nil {
		t.Errorf("list with an uncheckable mark: %d sessions and no error", len(listed))
	} else {
		reported("list command", err)
	}
	_, err = submitRetained(t, o, CallerHome, taskRequest())
	reported("a new submission", err)
}

// A missing journal whose session's mark cannot be checked proves neither a
// removal nor an execution not received yet: Info reports the mark's error
// rather than describing the session as preparing.
func TestInfoReportsAnUncheckableMarkBehindAMissingJournal(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	race := &interleaving{}
	c.interleave = race.at
	o := openOwner(t, c)
	v := waitPhase(t, o, submit(t, o).ID, phaseRetained)
	t.Cleanup(func() { _ = os.Chmod(c.Directory, 0o700) })
	var removeErr, chmodErr error
	race.arm(stepEntry, v.ID, func() {
		_, removeErr = o.Remove(t.Context(), v.ID, false)
		chmodErr = os.Chmod(c.Directory, 0o600)
	})
	_, err := o.Info(t.Context(), v.ID)
	if !race.fired() || removeErr != nil || chmodErr != nil {
		t.Fatalf("removal: ran %t, %v, %v", race.fired(), removeErr, chmodErr)
	}
	if _, lstatErr := os.Lstat(o.path(v.ID, removedSuffix)); !errors.Is(lstatErr, fs.ErrPermission) {
		t.Skipf("the directory's mode does not deny the check here: %v", lstatErr)
	}
	if code(err) != profile.CodeUnavailable || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("info with a missing journal and an uncheckable mark: %v (code %q)", err, code(err))
	}
}
