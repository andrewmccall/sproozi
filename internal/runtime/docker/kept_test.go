package docker

import (
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session"
)

// The Docker stop decides what it keeps by the shared rule
// (session.StopKeeps, ADR 0062) with the outcome of the rule it had before:
// it captures exactly when the earlier needsCapture did, and only an exited
// harness's failed capture keeps what the visit started from instead of
// failing the stop.
func TestNeedsCaptureKeepsTheEarlierDockerRule(t *testing.T) {
	t.Parallel()
	thread := "019a0000-0000-7000-8000-000000000001"
	for bits := range 64 {
		r := placementRecord{Turnless: bits&1 != 0}
		if bits&2 != 0 {
			r.Exited = &session.Exit{}
		}
		proves := bits&4 != 0
		if bits&8 != 0 {
			r.Thread = thread
		}
		if bits&16 != 0 {
			r.Launch.ThreadID, r.Launch.CheckpointDigest = thread, strings.Repeat("a", 64)
		}
		if bits&32 != 0 {
			r.CheckpointDigest = strings.Repeat("b", 64)
		}
		// The rule as it was before the shared one.
		exitedWithout := r.Exited != nil && (!proves || r.thread() == "")
		before := r.CheckpointDigest == "" && !r.Turnless && !exitedWithout
		if got := r.needsCapture(proves); got != before {
			t.Fatalf("record %d (proves %v): needsCapture %v, before %v", bits, proves, got, before)
		}
		// Before, a failed capture failed the stop exactly when nothing exited.
		if _, onFailure := r.kept(proves); before && (onFailure != 0) != (r.Exited != nil) {
			t.Fatalf("record %d: a failed capture falls back to %v", bits, onFailure)
		}
	}
}
