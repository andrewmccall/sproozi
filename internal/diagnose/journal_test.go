package diagnose_test

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/diagnose"
)

const (
	runA   = "run-a"
	runB   = "run-b"
	egress = "network.egress"
)

type recorder struct {
	mu     sync.Mutex
	events []audit.Event
}

func (r *recorder) Log(e audit.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

func denial(run, operation string, at time.Time) audit.Event {
	return audit.Event{Timestamp: at, RunUID: run, RunID: "ns/" + run, Operation: operation,
		Capability: egress, Service: "network", DenyReason: "destination not enabled"}
}

func TestJournalForwardsEveryEventAndKeepsOnlyExecutionDenials(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	next := &recorder{}
	journal := diagnose.NewJournal(next, diagnose.Limits{})
	allowed := denial(runA, "GET example.com:443", now)
	allowed.Allowed, allowed.DenyReason = true, ""
	anonymous := denial("", "CONNECT example.com:443", now)
	denied := denial(runA, "CONNECT docs.example.com:443", now)

	for _, e := range []audit.Event{allowed, anonymous, denied} {
		journal.Log(e)
	}

	if len(next.events) != 3 {
		t.Fatalf("forwarded %d events, want every event", len(next.events))
	}
	got := journal.Denials(runA, now.Add(-time.Minute), now)
	if len(got) != 1 || got[0].Event.Operation != denied.Operation || got[0].ID == "" {
		t.Fatalf("denials = %+v, want only the identified denial with an ID", got)
	}
}

func TestJournalNeverReturnsAnotherExecutionsDenials(t *testing.T) {
	now := time.Now().UTC()
	journal := diagnose.NewJournal(nil, diagnose.Limits{})
	journal.Log(denial(runA, "CONNECT a.example:443", now))
	journal.Log(denial(runB, "CONNECT b.example:443", now))

	for _, run := range []string{runA, runB} {
		got := journal.Denials(run, now.Add(-time.Minute), now)
		if len(got) != 1 || got[0].Event.RunUID != run {
			t.Fatalf("%s denials = %+v", run, got)
		}
		other := map[string]string{runA: runB, runB: runA}[run]
		if _, found := journal.Lookup(run, journal.Denials(other, now.Add(-time.Minute), now)[0].ID); found {
			t.Fatalf("%s could read %s's decision by ID", run, other)
		}
	}
	if got := journal.Denials("", now.Add(-time.Minute), now); len(got) != 0 {
		t.Fatalf("empty execution returned %+v", got)
	}
}

func TestJournalBoundsEntriesExecutionsAndAge(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	journal := diagnose.NewJournal(nil, diagnose.Limits{PerExecution: 2, Executions: 2, MaxAge: time.Minute})

	for i := range 3 {
		journal.Log(denial(runA, fmt.Sprintf("op-%d", i), now))
	}
	got := journal.Denials(runA, now.Add(-time.Minute), now)
	if len(got) != 2 || got[0].Event.Operation != "op-2" || got[1].Event.Operation != "op-1" {
		t.Fatalf("per-execution bound kept %+v, want newest two, newest first", got)
	}

	journal.Log(denial(runB, "b", now))
	journal.Log(denial("run-c", "c", now))
	if got := journal.Denials(runA, now.Add(-time.Minute), now); len(got) != 0 {
		t.Fatalf("least recently written execution was retained: %+v", got)
	}

	journal.Log(denial("run-c", "old", now.Add(-2*time.Minute)))
	for _, d := range journal.Denials("run-c", time.Time{}, now) {
		if d.Event.Operation == "old" {
			t.Fatal("denial older than MaxAge was returned")
		}
	}
	if got := journal.Denials(runB, now.Add(time.Second), now); len(got) != 0 {
		t.Fatalf("lookback window ignored: %+v", got)
	}
}
