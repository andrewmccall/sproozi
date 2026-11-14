package placement

import (
	"path/filepath"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

// The coordinator starts a first visit on Kubernetes the way it starts every
// first visit: Create retains the Git workspace, Recover prepares and
// activates it through Prepare, and Finalize stops it. This drives the real
// coordinator over the real placement; only the cluster and the Pod's
// supervisor and helper are fixtures.
func TestCoordinatorRunsFirstKubernetesVisitFromWorkspace(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		kind     harness.NativeKind
		turnless bool
	}{
		{"codex conversation", harness.NativeCodex, false},
		{"codex turnless", harness.NativeCodex, true},
		{"claude turnless", harness.NativeClaude, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p, kube, e, l, data := setupKind(t, test.kind)
			l = freshLaunch(t, e, l, data, "")
			l.Epoch = 1
			coordinator, err := session.Open(filepath.Join(t.TempDir(), "sessions"), map[string]session.Placement{l.Placement: p}, fixtureMax)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = coordinator.Close() })

			r, err := coordinator.Create(l, e.initial, "")
			if err != nil || r.Phase != session.PhasePreparing {
				t.Fatalf("create: %+v %v", r, err)
			}
			r, err = coordinator.Recover(t.Context(), l.ExecutionID)
			if err != nil || r.Phase != session.PhaseActive || r.Active == nil || kube.runCreates != 1 {
				t.Fatalf("recover: %+v %v", r, err)
			}

			e.turnless = test.turnless
			finalize := uuid.NewString()
			r, err = coordinator.Finalize(t.Context(), l.ExecutionID, finalize)
			if err != nil || r.Phase != session.PhaseIdle || r.Active != nil || len(r.Visits) != 1 {
				t.Fatalf("finalize: %+v %v", r, err)
			}
			visit := r.Visits[0]
			if !visit.ActualStopped || visit.Receipt == nil || r.Operations[0].Incarnation == nil || visit.Incarnation != *r.Operations[0].Incarnation {
				t.Fatalf("finalized visit: %+v", visit)
			}
			if test.turnless {
				if r.ThreadID != "" || visit.CheckpointDigest != "" || r.Checkpoint.Metadata.SHA256 != "" || r.Workspace == "" {
					t.Fatalf("turnless visit kept a conversation: %+v", r)
				}
			} else if r.ThreadID != fixtureThread || visit.CheckpointDigest != e.meta.SHA256 || r.Checkpoint.Metadata.SHA256 != e.meta.SHA256 {
				t.Fatalf("conversation not kept: %+v", r)
			}
			if replay, err := coordinator.Finalize(t.Context(), l.ExecutionID, finalize); err != nil || replay.Phase != session.PhaseIdle || len(replay.Visits) != 1 {
				t.Fatalf("finalize replay: %+v %v", replay, err)
			}
		})
	}
}
