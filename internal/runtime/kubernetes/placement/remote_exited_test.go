package placement

import (
	"errors"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

// A visit home moved to a cluster whose harness exits by itself is noticed
// over the placement protocol, as home's exit watcher asks, and the stop the
// watcher then makes through home's coordinator ends with what the shared
// exit rule keeps (ADR 0062). Home moves only resumed visits to a cluster:
// the protocol carries no first visit, which only the executor's own owner
// starts there.
func TestHomeNoticesAndStopsARemoteVisitWhoseHarnessExited(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		kind       harness.NativeKind
		captureErr error
		// captured: the session keeps the checkpoint captured from the
		// visit; otherwise the one the visit started from.
		captured bool
	}{
		{"Codex visit keeps its conversation", harness.NativeCodex, nil, true},
		{"Codex visit whose capture fails keeps its input", harness.NativeCodex, errors.New("capture failed"), false},
		{"Claude visit keeps its input", harness.NativeClaude, nil, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p, kube, e, l, data := setupKind(t, test.kind)
			l.Epoch = 1
			coordinator, client, _ := remoteHome(t, p, l)
			if _, err := coordinator.Create(l, data, ""); err != nil {
				t.Fatal(err)
			}
			r, err := coordinator.Recover(t.Context(), l.ExecutionID)
			if err != nil || r.Phase != session.PhaseActive {
				t.Fatalf("remote visit: %s %v", r.Phase, err)
			}
			var observer session.ExitObserver = client
			if exited, err := observer.Exited(t.Context(), r.Active.Launch, r.Active.Incarnation); err != nil || exited {
				t.Fatalf("a running harness reported as exited: %v %v", exited, err)
			}
			work, meta := cachedFixtureArtifact(t, test.kind, "work of this visit\n")
			e.data, e.captureErr = work, test.captureErr
			endNative(t, p, kube, l, 0)
			e.logs = exitedReport(0, "")
			if exited, err := observer.Exited(t.Context(), r.Active.Launch, r.Active.Incarnation); err != nil || !exited {
				t.Fatalf("an exited harness not reported over the protocol: %v %v", exited, err)
			}
			r, err = coordinator.Finalize(t.Context(), l.ExecutionID, uuid.NewString())
			if err != nil || r.Phase != session.PhaseIdle || len(r.Visits) != 1 || r.Visits[0].Exit == nil {
				t.Fatalf("stop of an exited remote visit: %s %v", r.Phase, err)
			}
			want := l.CheckpointDigest
			if test.captured {
				want = meta.SHA256
			}
			if kept := r.Checkpoint.Metadata.SHA256; kept != want || r.ThreadID != fixtureThread {
				t.Fatalf("kept %q of thread %q, want %q", kept, r.ThreadID, want)
			}
			if exited, err := observer.Exited(t.Context(), r.Visits[0].Launch, r.Visits[0].Incarnation); err != nil || exited {
				t.Fatalf("a stopped visit reported as exited: %v %v", exited, err)
			}
		})
	}
}
