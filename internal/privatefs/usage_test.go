package privatefs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const pendingName = ".pending-0123456789abcdef0123456789abcdef"

func writeSized(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, n), 0o600); err != nil {
		t.Fatal(err)
	}
}

// at runs fn once, when usage reaches the entry at path: after its directory
// was listed and before the entry is examined.
func at(path string, fn func()) func(string) {
	return func(p string) {
		if p == path {
			fn()
		}
	}
}

// A replacement that completes between the listing and the examination of
// its temporary file is a write after the listing: the sum is what the
// listing found, with the replaced file at whatever size it has when it is
// examined, and no error.
func TestUsageSkipsAReplacementThatCompletesDuringTheWalk(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		target string
		want   int64
	}{
		// a.json is examined after the rename and has the new size; the
		// temporary file's own bytes were not counted.
		{"over a listed file", "a.json", 5 + 7},
		// b.json was not in the listing, as if written just after it.
		{"to a new name", "b.json", 3 + 7},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeSized(t, filepath.Join(dir, "a.json"), 3)
			writeSized(t, filepath.Join(dir, "z.json"), 7)
			temp := filepath.Join(dir, pendingName)
			writeSized(t, temp, 5)
			size, err := usage(dir, nil, at(temp, func() {
				if err := os.Rename(temp, filepath.Join(dir, c.target)); err != nil {
					t.Error(err)
				}
			}))
			if err != nil || size != c.want {
				t.Fatalf("usage = %d, %v; want %d, no error", size, err, c.want)
			}
		})
	}
}

// A file removed, and a scratch directory removed with its contents, between
// the listing and the examination are no longer kept and are not counted.
func TestUsageSkipsWhatIsRemovedDuringTheWalk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeSized(t, filepath.Join(dir, "a.json"), 3)
	removed := filepath.Join(dir, "removed.json")
	writeSized(t, removed, 100)
	scratch := filepath.Join(dir, "scratch")
	if err := os.Mkdir(scratch, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSized(t, filepath.Join(scratch, "file"), 1000)
	size, err := usage(dir, nil, func(p string) {
		if p == removed || p == scratch {
			if err := os.RemoveAll(p); err != nil {
				t.Error(err)
			}
		}
	})
	if err != nil || size != 3 {
		t.Fatalf("usage = %d, %v; want 3, no error", size, err)
	}
}

// A temporary file that still exists is counted: for a moment the replaced
// file is counted twice, which only overstates what is kept. Names skip
// accepts are not counted.
func TestUsageCountsWhatItFinds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeSized(t, filepath.Join(dir, "a.json"), 3)
	writeSized(t, filepath.Join(dir, pendingName), 5)
	writeSized(t, filepath.Join(dir, "a.removed"), 100)
	if err := os.Mkdir(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeSized(t, filepath.Join(dir, "sub", "b"), 7)
	size, err := Usage(dir, func(name string) bool { return strings.HasSuffix(name, ".removed") })
	if want := int64(3 + 5 + 7); err != nil || size != want {
		t.Fatalf("Usage = %d, %v; want %d, no error", size, err, want)
	}
}

// Only an entry the listing found and that is gone when examined is
// skipped: a root that is missing, or that goes after it was examined, still
// fails the sum.
func TestUsageFailsWithoutItsRoot(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "missing")
	if _, err := Usage(missing, nil); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Usage of a missing root: %v, want not exist", err)
	}
	root := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := usage(root, nil, at(root, func() {
		if err := os.Remove(root); err != nil {
			t.Error(err)
		}
	}))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("usage of a root removed during the walk: %v, want not exist", err)
	}
}
