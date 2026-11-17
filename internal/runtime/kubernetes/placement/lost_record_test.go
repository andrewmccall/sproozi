package placement

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session"
)

// An executor that lost a stopped visit's record, and whose owner then
// repeated the visit's Prepare, holds a record with the launch alone; the
// replayed Prepare is refused. Inspect, Stop and Abort read the stopped
// registration again and settle from its receipt while the output file it
// names survived. With the output file lost too, nothing can return the
// checkpoint the receipt names: each fails, and none reports the visit
// stopped, until the file is put back (an executor's storage restored from a
// backup); cancelling cannot end the visit either, since an abort of a
// stopped visit reports the same stop.
func TestAStoppedVisitWhoseRecordWasLostSettlesFromItsReceipt(t *testing.T) {
	t.Parallel()
	for _, output := range []string{"kept", "lost"} {
		t.Run(output, func(t *testing.T) {
			t.Parallel()
			p, _, e, l, data := setup(t)
			i := activeVisit(t, p, l, data)
			if _, err := p.Stop(t.Context(), l, i); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(p.config.Directory, l.AllocationID+recordSuffix)); err != nil {
				t.Fatal(err)
			}
			outputPath := p.artifactPath(l.AllocationID, "output")
			kept, err := os.ReadFile(outputPath)
			if err != nil {
				t.Fatal(err)
			}
			if output == "lost" {
				if err = os.Remove(outputPath); err != nil {
					t.Fatal(err)
				}
			}
			if _, err = p.Prepare(t.Context(), l, data); !errors.Is(err, session.ErrRecordContradicted) && !isConflict(err) {
				t.Fatalf("replayed Prepare of a stopped visit: %v", err)
			}
			var contradicted *contradiction
			o, inspectErr := p.Inspect(t.Context(), l)
			s, stopErr := p.Stop(t.Context(), l, i)
			a, abortErr := p.Abort(t.Context(), l, i)
			for name, err := range map[string]error{"inspect": inspectErr, "stop": stopErr, "abort": abortErr} {
				if errors.As(err, &contradicted) {
					t.Errorf("%s reported the contradiction: %v", name, err)
				}
			}
			if output == "kept" {
				if inspectErr != nil || o.State != session.PlacementStopped || o.Stopped.CheckpointDigest != e.meta.SHA256 {
					t.Errorf("inspect: %s %v", o.State, inspectErr)
				}
				if stopErr != nil || s.CheckpointDigest != e.meta.SHA256 {
					t.Errorf("stop: %q %v", s.CheckpointDigest, stopErr)
				}
				if abortErr != nil || a.Stopped == nil || a.Stopped.CheckpointDigest != e.meta.SHA256 {
					t.Errorf("abort: %+v %v", a.Stopped, abortErr)
				}
				requireValidRecord(t, p, l)
				return
			}
			if inspectErr == nil && o.State == session.PlacementStopped || stopErr == nil || abortErr == nil {
				t.Fatalf("a stopped visit whose output was lost settled: inspect %s %v, stop %v, abort %v", o.State, inspectErr, stopErr, abortErr)
			}
			if err = os.WriteFile(outputPath, kept, 0o600); err != nil {
				t.Fatal(err)
			}
			if s, err = p.Stop(t.Context(), l, i); err != nil || s.CheckpointDigest != e.meta.SHA256 {
				t.Fatalf("stop once the output was restored: %q %v", s.CheckpointDigest, err)
			}
		})
	}
}
