package work

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

// driveToEnd runs the session's driver as dispatch does and requires it to
// return: nothing retries for ever and nothing stays blocked.
func driveToEnd(t *testing.T, e *driverEnv) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.o.drive(ctx, e.id, make(chan struct{}))
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the driver is still driving a start its placement refused")
	}
}

// A placement that refuses a session's first visit for good ends its start
// at once, deadline or none, with a problem that names the refusal and not
// the deadline (ADR 0067). A task is cancelled with it; an interactive
// session is retained with its work, and its owner resumes it. Prepare is
// asked once: nothing retries it.
func TestDriverEndsAStartItsPlacementRefuses(t *testing.T) {
	t.Parallel()
	for _, interactive := range []bool{false, true} {
		for _, clock := range []driverClock{clockEarly, clockNone} {
			name := map[bool]string{false: "task", true: "interactive"}[interactive] + "/" + string(clock)
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				e := newDriverEnv(t, interactive, clock)
				e.create(t)
				e.refuse()
				driveToEnd(t, e)
				v, err := e.o.Info(t.Context(), e.id)
				if err != nil {
					t.Fatal(err)
				}
				r, err := e.o.read(e.id)
				if err != nil {
					t.Fatal(err)
				}
				rec, err := e.o.config.Coordinator.Inspect(e.id)
				if err != nil {
					t.Fatal(err)
				}
				want := "it can never start: " + fixtureRefusal
				if v.Problem != want {
					t.Fatalf("problem %q, want %q", v.Problem, want)
				}
				if calls := e.p.counts(); !strings.Contains(calls, "prepare=1,") {
					t.Fatalf("the refused start was prepared again: %s", calls)
				}
				if !interactive {
					if v.Phase != phaseCancelled || rec.Phase != session.PhaseCancelled || r.Finished ||
						rec.CancelOperation != derivedOperation(e.id, "start-refused") {
						t.Fatalf("refused task: %s, journal %s, cancel %s", v.Phase, rec.Phase, e.opName(rec.CancelOperation))
					}
					return
				}
				if v.Phase != PhaseRetained || rec.Phase != session.PhaseIdle || r.CancelOperation != "" {
					t.Fatalf("refused interactive session: %s, journal %s", v.Phase, rec.Phase)
				}
				e.fixture(func(p *fixturePlacement) { p.refusePrepare = nil })
				resumed, err := e.o.Resume(t.Context(), e.id, uuid.NewString(), "")
				if err != nil || resumed.Problem != "" {
					t.Fatalf("resume of the refused interactive session: %+v %v", resumed, err)
				}
			})
		}
	}
}

// Until the refused start is shown to have ended, a task keeps being
// cancelled after the backoff, and an interactive session waits for its
// owner: neither retries Prepare at once, and the problem says what to run.
func TestDriverWaitsForProofThatARefusedStartEnded(t *testing.T) {
	t.Parallel()
	for _, interactive := range []bool{false, true} {
		t.Run(map[bool]string{false: "task", true: "interactive"}[interactive], func(t *testing.T) {
			t.Parallel()
			e := newDriverEnv(t, interactive, clockNone)
			e.create(t)
			e.refuse()
			e.p.mu.Lock()
			e.p.failInspectAfterPrepare = true
			e.p.mu.Unlock()
			n := e.o.step(t.Context(), e.id)
			r, err := e.o.read(e.id)
			if err != nil {
				t.Fatal(err)
			}
			end := map[bool]string{false: "cancel this session", true: "recover or cancel this session"}[interactive]
			want := "it can never start: " + fixtureRefusal + "; the executor has not yet shown that nothing runs there, so " + end
			if r.Problem != want {
				t.Fatalf("problem %q, want %q", r.Problem, want)
			}
			if interactive {
				if n != nextDone {
					t.Fatalf("unproven refused interactive start: %s, want done", nextNames[n])
				}
				return
			}
			if n != nextAgain {
				t.Fatalf("unproven refused task: %s, want again", nextNames[n])
			}
			if n = e.o.step(t.Context(), e.id); n != nextWait {
				t.Fatalf("cancel of the unproven refused task: %s, want wait", nextNames[n])
			}
			if r, err = e.o.read(e.id); err != nil || r.Problem != want {
				t.Fatalf("problem after the cancel %q %v", r.Problem, err)
			}
			e.p.mu.Lock()
			e.p.failInspectAfterPrepare = false
			e.p.mu.Unlock()
			driveToEnd(t, e)
			v, err := e.o.Info(t.Context(), e.id)
			if err != nil || v.Phase != phaseCancelled || v.Problem != want {
				t.Fatalf("refused task once its end is proven: %+v %v", v, err)
			}
		})
	}
}
