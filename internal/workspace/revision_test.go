package workspace

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRecordedRevisionProducesCleanFreshWorkspaceWithoutChangingSource(t *testing.T) {
	root := newFixture(t)
	head := fixtureGit(t, root, "rev-parse", "HEAD")
	data, err := Capture(t.Context(), root, nil, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	meta, err := Inspect(data, testLimit)
	if err != nil || meta.Revision != head || meta.ObjectFormat != "sha1" || len(meta.SHA256) != 64 {
		t.Fatalf("Recorded workspace identity: %#v %v", meta, err)
	}
	fresh, err := AtRevision(t.Context(), data, head, t.TempDir(), testLimit)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "fresh")
	if err = Restore(t.Context(), fresh, target, testLimit); err != nil {
		t.Fatal(err)
	}
	text, err := os.ReadFile(filepath.Join(target, "note.txt"))
	if err != nil || string(text) != "original\n" || fixtureGit(t, target, "status", "--porcelain") != "" {
		t.Fatalf("Fresh execution inherited unfinished state: %q %v", text, err)
	}
	if fixtureGit(t, target, "rev-parse", "HEAD") != head {
		t.Fatal("Fresh execution changed the recorded commit")
	}
	source, err := os.ReadFile(filepath.Join(root, "note.txt"))
	if err != nil || string(source) != "unstaged\n" || fixtureGit(t, root, "show", ":note.txt") != "staged" {
		t.Fatal("Revision reproduction changed the original unfinished workspace")
	}
	if _, err = AtRevision(t.Context(), data, head[:8], t.TempDir(), testLimit); err == nil {
		t.Fatal("A short ambiguous revision selected a workspace")
	}
}
