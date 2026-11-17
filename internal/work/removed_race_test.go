//nolint:goconst // Keep independently stated literal behavior expectations.
package work

import (
	"errors"
	"os"
	"sync"
	"testing"

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
		pulled      bool
		step        string
		want        profile.Code
		run         func(o *Owner, s removedSession) error
	}{
		{name: "info", step: stepEntry, want: profile.CodeInvalid, run: func(o *Owner, s removedSession) error {
			_, err := o.Info(ctx, s.ID)
			return err
		}},
		{name: "list", step: stepEntry, run: func(o *Owner, s removedSession) error {
			listed, err := o.List(ctx, SessionQuery{})
			for _, v := range listed {
				if v.ID == s.ID {
					return errors.New("listed as " + string(v.Phase))
				}
			}
			return err
		}},
		{name: "recover", step: stepEntry, want: profile.CodeInvalid, run: func(o *Owner, s removedSession) error {
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
		{name: "attach", interactive: true, step: stepEntry, want: profile.CodeInvalid, run: func(o *Owner, s removedSession) error {
			_, err := o.Attach(ctx, s.ID)
			return err
		}},
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
			} else {
				s.request = taskRequest()
				v, err := o.Submit(t.Context(), s.request)
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
			assertRemoved(t, c, o, s.ID, s.ExecutionID)
			after := tc.run(o, s)
			if code(during) != tc.want || errText(during) != errText(after) {
				t.Fatalf("%s on a session removed at its %s step: %v (code %q); on a removed session: %v",
					tc.name, tc.step, during, code(during), after)
			}
		})
	}
}
