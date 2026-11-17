package work

import (
	"context"
	"errors"
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
			name := map[bool]string{false: string(ModeTask), true: string(ModeInteractive)}[interactive] + "/" + string(clock)
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

// Until the refused start is shown to have ended, a task is recovered
// again after the backoff, which refuses it again and repeats the proof,
// and an interactive session waits for its owner's recover or cancel;
// neither retries at once, and the problem says what happens next. Once the
// proof holds, the start ends with the refusal's reason. (The fixture's
// failure also fails the next recover's Inspect, which records that
// failure instead, as any failed recovery does.)
func TestDriverWaitsForProofThatARefusedStartEnded(t *testing.T) {
	t.Parallel()
	for _, interactive := range []bool{false, true} {
		t.Run(map[bool]string{false: string(ModeTask), true: string(ModeInteractive)}[interactive], func(t *testing.T) {
			t.Parallel()
			e := newDriverEnv(t, interactive, clockNone)
			e.create(t)
			e.refuse()
			e.p.mu.Lock()
			e.p.failInspectAfterPrepare = true
			e.p.mu.Unlock()
			then := map[bool]string{false: " and tries again", true: ", so recover or cancel this session"}[interactive]
			want := "it can never start: " + fixtureRefusal + "; the executor has not yet shown that nothing runs there" + then
			wantNext := map[bool]next{false: nextWait, true: nextDone}[interactive]
			n := e.o.step(t.Context(), e.id)
			r, err := e.o.read(e.id)
			if err != nil {
				t.Fatal(err)
			}
			if r.Problem != want {
				t.Fatalf("problem %q, want %q", r.Problem, want)
			}
			if n != wantNext {
				t.Fatalf("unproven refused start: %s, want %s", nextNames[n], nextNames[wantNext])
			}
			if rec, err := e.o.config.Coordinator.Inspect(e.id); err != nil || rec.Phase != session.PhasePreparing {
				t.Fatalf("unproven refused start was recorded: %s %v", rec.Phase, err)
			}
			e.p.mu.Lock()
			e.p.failInspectAfterPrepare = false
			e.p.mu.Unlock()
			driveToEnd(t, e)
			v, err := e.o.Info(t.Context(), e.id)
			wantPhase := map[bool]Phase{false: phaseCancelled, true: PhaseRetained}[interactive]
			if err != nil || v.Phase != wantPhase || v.Problem != "it can never start: "+fixtureRefusal {
				t.Fatalf("refused start once its end is proven: %+v %v", v, err)
			}
		})
	}
}

// If the executor stops after the coordinator ended a refused start but
// before the driver recorded why, the execution is idle with no problem.
// The driver still tells the refusal from the deadline by the coordinator's
// clock, with no deadline as here: a task is cancelled as refused, and an
// interactive session is retained and shown as refused, without the reason
// the journal does not keep (ADR 0067).
func TestDriverEndsARefusedStartWhoseProblemWasNotRecorded(t *testing.T) {
	t.Parallel()
	for _, interactive := range []bool{false, true} {
		t.Run(map[bool]string{false: string(ModeTask), true: string(ModeInteractive)}[interactive], func(t *testing.T) {
			t.Parallel()
			e := newDriverEnv(t, interactive, clockNone)
			e.create(t)
			e.refuse()
			if rec, err := e.o.config.Coordinator.Recover(t.Context(), e.id); !errors.Is(err, session.ErrStartEnded) ||
				rec.Phase != session.PhaseIdle {
				t.Fatalf("refused start: %s %v", rec.Phase, err)
			}
			driveToEnd(t, e)
			v, err := e.o.Info(t.Context(), e.id)
			if err != nil {
				t.Fatal(err)
			}
			rec, err := e.o.config.Coordinator.Inspect(e.id)
			if err != nil {
				t.Fatal(err)
			}
			if v.Problem != "it can never start: its placement refused it" {
				t.Fatalf("problem %q", v.Problem)
			}
			if interactive && (v.Phase != PhaseRetained || rec.CancelOperation != "") ||
				!interactive && (v.Phase != phaseCancelled || rec.CancelOperation != derivedOperation(e.id, "start-refused")) {
				t.Fatalf("refused start whose problem was not recorded: %s, cancel %s", v.Phase, e.opName(rec.CancelOperation))
			}
		})
	}
}

// adminCheck is a placement's unproven end that only an administrator can
// settle, as the Kubernetes placement's for a replaced run is: its message
// wraps a chain, and UnprovenEnd states the cause alone.
type adminCheck struct{ sentence string }

func (e *adminCheck) Error() string {
	return "placement: " + e.sentence + ": session profile: conflict"
}
func (e *adminCheck) UnprovenEnd() string { return e.sentence }

const fixtureAdminCheck = "AgentRun session-0 now has UID b, but its session registration bound UID a; " +
	"an administrator replaced it: confirm no Pod labelled sproozi.com/agentrun-uid=a remains, " +
	"then delete AgentRun session-0 and recover this session"

// A refused start whose end stays unproven for a cause only an
// administrator can clear says that cause, which names what clears it, in
// its problem, without the errors it is wrapped in. The driver stops, task
// or interactive, rather than retry what only an administrator can change;
// each recover checks again, and the executor logs the cause once however
// often it is checked. Once the cause is cleared, a recover ends the start
// (ADR 0067).
func TestDriverNamesWhyARefusedStartsEndIsUnproven(t *testing.T) {
	t.Parallel()
	for _, interactive := range []bool{false, true} {
		t.Run(map[bool]string{false: string(ModeTask), true: string(ModeInteractive)}[interactive], func(t *testing.T) {
			t.Parallel()
			e := newDriverEnv(t, interactive, clockNone)
			e.create(t)
			e.refuse()
			e.p.mu.Lock()
			e.p.failInspectAfterPrepare, e.p.inspectErr = true, &adminCheck{fixtureAdminCheck}
			e.p.mu.Unlock()
			want := "it can never start: " + fixtureRefusal + "; the executor cannot show that nothing runs there: " +
				fixtureAdminCheck
			// The driver's step, then two recovers, as the owner's would.
			for range 3 {
				if n := e.o.step(t.Context(), e.id); n != nextDone {
					t.Fatalf("unproven end only an administrator can clear: %s, want done", nextNames[n])
				}
				r, err := e.o.read(e.id)
				if err != nil {
					t.Fatal(err)
				}
				if r.Problem != want {
					t.Fatalf("problem %q, want %q", r.Problem, want)
				}
			}
			e.logMu.Lock()
			logged := e.logs.String()
			e.logMu.Unlock()
			if strings.Count(logged, fixtureAdminCheck) != 1 || strings.Contains(logged, "session profile: conflict") {
				t.Fatalf("log %q: want the cause once, alone", logged)
			}
			e.p.mu.Lock()
			e.p.failInspectAfterPrepare = false
			e.p.mu.Unlock()
			if _, err := e.o.Recover(t.Context(), e.id); err != nil {
				t.Fatal(err)
			}
			driveToEnd(t, e)
			v, err := e.o.Info(t.Context(), e.id)
			wantPhase := map[bool]Phase{false: phaseCancelled, true: PhaseRetained}[interactive]
			if err != nil || v.Phase != wantPhase || v.Problem != "it can never start: "+fixtureRefusal {
				t.Fatalf("refused start once its cause was cleared: %+v %v", v, err)
			}
			if n := loggedCauses(e.o); n != 0 {
				t.Fatalf("%d logged causes kept after the start ended", n)
			}
		})
	}
}

// loggedCauses counts the unproven causes o remembers having logged.
func loggedCauses(o *Owner) int {
	n := 0
	o.unproven.Range(func(any, any) bool { n++; return true })
	return n
}

// Removing a session forgets the causes the executor logged for it.
func TestRemovingASessionForgetsItsLoggedCauses(t *testing.T) {
	t.Parallel()
	e := newDriverEnv(t, true, clockNone)
	e.create(t)
	e.refuse()
	e.p.mu.Lock()
	e.p.failInspectAfterPrepare, e.p.inspectErr = true, &adminCheck{fixtureAdminCheck}
	e.p.mu.Unlock()
	e.o.step(t.Context(), e.id)
	if n := loggedCauses(e.o); n != 1 {
		t.Fatalf("%d logged causes, want 1", n)
	}
	if _, err := e.o.Cancel(t.Context(), e.id, uuid.NewString()); err != nil {
		t.Logf("cancel of the unproven start: %v", err)
	}
	e.p.mu.Lock()
	e.p.failInspectAfterPrepare = false
	e.p.mu.Unlock()
	if _, err := e.o.Recover(t.Context(), e.id); err != nil {
		t.Logf("recover of the cancellation: %v", err)
	}
	if _, err := e.o.Remove(t.Context(), e.id, false); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if n := loggedCauses(e.o); n != 0 {
		t.Fatalf("%d logged causes kept after the session was removed", n)
	}
}
