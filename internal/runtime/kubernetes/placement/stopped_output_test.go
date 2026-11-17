package placement

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
)

// A stopped visit returns the output file only when it is the checkpoint
// the registration's receipt names. Another valid checkpoint of the same
// harness and thread, with another digest, in its place is refused: by the
// stop's replay, by Inspect and by Abort (whose observation, as for every
// failed abort of a stopped visit, says stopped beside its error), whether
// the record still names the receipt's digest or lost it and takes it from
// the receipt.
func TestStoppedVisitRefusesAnotherCheckpointInPlaceOfItsOutput(t *testing.T) {
	t.Parallel()
	for _, record := range []string{"intact", "lost-its-facts"} {
		t.Run(record, func(t *testing.T) {
			t.Parallel()
			p, _, e, l, data := setup(t)
			i := activeVisit(t, p, l, data)
			if _, err := p.Stop(t.Context(), l, i); err != nil {
				t.Fatal(err)
			}
			r := registration(t, p, l)
			if r.Phase != registry.Stopped || r.Receipt.CheckpointDigest != e.meta.SHA256 {
				t.Fatalf("stop: %+v", r)
			}
			other, meta := cachedFixtureArtifact(t, harness.NativeCodex, "another checkpoint of the same thread")
			if meta.SHA256 == r.Receipt.CheckpointDigest || meta.ThreadID != fixtureThread || meta.Harness != l.Harness {
				t.Fatalf("fixture checkpoint is not another one of the same thread: %+v", meta)
			}
			if err := os.WriteFile(filepath.Join(p.config.Directory, l.AllocationID+"-output.checkpoint"), other, 0o600); err != nil {
				t.Fatal(err)
			}
			if record == "lost-its-facts" {
				contradict(t, p, l, func(r *retained) { r.NativeQuiet, r.WriterFenced, r.OutputDigest = false, false, "" })
			}
			if s, err := p.Stop(t.Context(), l, i); err == nil || s.CheckpointDigest != "" || len(s.Checkpoint) != 0 {
				t.Errorf("stop replay returned %q: %v", s.CheckpointDigest, err)
			}
			if o, err := p.Inspect(t.Context(), l); err == nil || o.State == session.PlacementStopped || o.Stopped != nil {
				t.Errorf("inspect reported %s: %v", o.State, err)
			}
			if o, err := p.Abort(t.Context(), l, i); err == nil || o.Stopped != nil && o.Stopped.CheckpointDigest != "" {
				t.Errorf("abort reported %+v: %v", o.Stopped, err)
			}
		})
	}
}
