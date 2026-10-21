package diagnose_test

import (
	"fmt"
	"strings"
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

// Denials carrying huge workload-chosen text across many executions keep the
// journal within its byte budget, and each retained field is bounded.
func TestJournalBoundsRetainedBytes(t *testing.T) {
	journal := diagnose.NewJournal(nil, diagnose.Limits{MaxBytes: 1 << 20})
	now := time.Now()
	huge := strings.Repeat("a", 64<<10)
	for run := range 400 {
		for range 64 {
			e := denial(fmt.Sprintf("run-%d", run), "CONNECT "+huge, now)
			e.DenyReason = "destination " + huge + " is not enabled at this gateway"
			journal.Log(e)
		}
	}
	retained := 0
	for run := range 400 {
		for _, d := range journal.Denials(fmt.Sprintf("run-%d", run), now.Add(-time.Minute), now) {
			if len(d.Event.Operation) > 520 || len(d.Event.DenyReason) > 520 {
				t.Fatalf("retained field not bounded: %d, %d bytes", len(d.Event.Operation), len(d.Event.DenyReason))
			}
			retained += len(d.Event.Operation) + len(d.Event.DenyReason) + 256
		}
	}
	if retained > 1<<20+64*2048 {
		t.Fatalf("journal retains about %d bytes, budget 1 MiB", retained)
	}
	if journal.Denials("run-399", now.Add(-time.Minute), now) == nil {
		t.Fatal("the most recent execution's denials were evicted")
	}
}

// Expired denials are removed when an execution records a new one, not only
// hidden from queries.
func TestJournalDropsExpiredDenials(t *testing.T) {
	journal := diagnose.NewJournal(nil, diagnose.Limits{MaxAge: time.Minute})
	start := time.Now()
	journal.Log(denial(runA, "CONNECT old.example:443", start))
	journal.Log(denial(runA, "CONNECT new.example:443", start.Add(2*time.Minute)))
	got := journal.Denials(runA, start.Add(-time.Hour), start.Add(2*time.Minute))
	if len(got) != 1 {
		t.Fatalf("retained %d denials, want only the unexpired one", len(got))
	}
	if _, ok := journal.Lookup(runA, got[0].ID); !ok {
		t.Fatal("lookup lost the retained denial")
	}
}
