package session

import "testing"

// The exit rule both placements share (ADR 0062): what a stopped visit
// keeps, and what it keeps when its capture fails.
func TestStopKeeps(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		end       VisitEnd
		keep      Kept
		onFailure Kept
	}{
		{"graceful resumed visit", VisitEnd{Conversation: true}, KeptCapture, 0},
		{"graceful first visit with a conversation", VisitEnd{First: true, Conversation: true}, KeptCapture, 0},
		{"graceful first visit of a harness that proves completion", VisitEnd{First: true, ProvesCompletion: true, Conversation: true}, KeptCapture, 0},
		{"turnless first visit", VisitEnd{First: true, Turnless: true}, KeptWorkspace, 0},
		{"turnless first visit that exited", VisitEnd{First: true, Turnless: true, Exited: true, ProvesCompletion: true}, KeptWorkspace, 0},
		{"exited resumed visit whose state proves completion", VisitEnd{Exited: true, ProvesCompletion: true, Conversation: true}, KeptCapture, KeptInput},
		{"exited first visit whose state proves completion", VisitEnd{First: true, Exited: true, ProvesCompletion: true, Conversation: true}, KeptCapture, KeptWorkspace},
		{"exited resumed visit whose state proves nothing", VisitEnd{Exited: true, Conversation: true}, KeptInput, 0},
		// A conversation bound before the harness exited is never claimed
		// from a stopped container whose state proves nothing (ADR 0011).
		{"exited first visit with a bound conversation whose state proves nothing", VisitEnd{First: true, Exited: true, Conversation: true}, KeptWorkspace, 0},
		{"exited first visit without a conversation", VisitEnd{First: true, Exited: true, ProvesCompletion: true}, KeptWorkspace, 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			keep, onFailure := StopKeeps(test.end)
			if keep != test.keep || onFailure != test.onFailure {
				t.Fatalf("StopKeeps(%+v) = %v, %v; want %v, %v", test.end, keep, onFailure, test.keep, test.onFailure)
			}
		})
	}
}

// Over every input: a stop that does not capture keeps what the visit
// started from, only an exited visit's capture falls back, to the same, and
// a stopped container whose state proves nothing is never captured.
func TestStopKeepsOverEveryInput(t *testing.T) {
	t.Parallel()
	for bits := range 32 {
		e := VisitEnd{First: bits&1 != 0, Turnless: bits&2 != 0, Exited: bits&4 != 0, ProvesCompletion: bits&8 != 0, Conversation: bits&16 != 0}
		keep, onFailure := StopKeeps(e)
		start := KeptInput
		if e.First {
			start = KeptWorkspace
		}
		switch {
		case keep != KeptCapture && (keep != start || onFailure != 0):
			t.Fatalf("%+v keeps %v, %v without capturing", e, keep, onFailure)
		case keep == KeptCapture && e.Exited && onFailure != start, keep == KeptCapture && !e.Exited && onFailure != 0:
			t.Fatalf("%+v falls back to %v", e, onFailure)
		case keep == KeptCapture && e.Exited && (!e.ProvesCompletion || !e.Conversation):
			t.Fatalf("%+v captures a stopped container that proves no conversation", e)
		case keep == KeptCapture && e.Turnless:
			t.Fatalf("%+v captures a turnless visit", e)
		}
	}
}
