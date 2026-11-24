package privatefs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

var onWindows = runtime.GOOS == "windows"

const pendingName = ".pending-0123456789abcdef0123456789abcdef"

func writeSized(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.WriteFile(path, make([]byte, n), 0o600); err != nil {
		t.Fatal(err)
	}
}

// walk is usage with the real check that an entry is gone.
func walk(root string, listed func(string)) (int64, error) {
	return usage(root, nil, listed, gone)
}

// The walk asks whether an entry is really gone before it skips one that
// failed as not existing, and fails the sum if it is not.
func TestUsageConsultsGoneBeforeSkipping(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (dir, removed string, hook func(string)) {
		dir = t.TempDir()
		writeSized(t, filepath.Join(dir, "a.json"), 3)
		removed = filepath.Join(dir, "removed.json")
		writeSized(t, removed, 100)
		return dir, removed, at(removed, func() {
			if err := os.Remove(removed); err != nil {
				t.Error(err)
			}
		})
	}
	t.Run("not gone fails the sum", func(t *testing.T) {
		t.Parallel()
		dir, _, hook := setup(t)
		size, err := usage(dir, nil, hook, func(string, string) bool { return false })
		if !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("usage = %d, %v; want a not exist error", size, err)
		}
	})
	t.Run("gone is skipped", func(t *testing.T) {
		t.Parallel()
		dir, removed, hook := setup(t)
		var asked []string
		size, err := usage(dir, nil, hook, func(root, path string) bool {
			if root != dir {
				t.Errorf("asked with root %s, want %s", root, dir)
			}
			asked = append(asked, path)
			return true
		})
		if err != nil || size != 3 {
			t.Fatalf("usage = %d, %v; want 3, no error", size, err)
		}
		if len(asked) != 1 || asked[0] != removed {
			t.Fatalf("asked about %v, want only %s", asked, removed)
		}
	})
}

// A root removed after it was listed is not a reason to skip what was under
// it: gone does not climb above the root, so the sum fails rather than
// returning a partial sum.
func TestUsageFailsWhenTheRootIsRemovedDuringTheWalk(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "root")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSized(t, filepath.Join(root, "a.json"), 3)
	child := filepath.Join(root, "a.json")
	size, err := walk(root, at(child, func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	}))
	if err == nil {
		t.Fatalf("usage = %d, nil; want an error for a root removed during the walk", size)
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
			size, err := walk(dir, at(temp, func() {
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
	size, err := walk(dir, func(p string) {
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
	_, err := walk(root, at(root, func() {
		if err := os.Remove(root); err != nil {
			t.Error(err)
		}
	}))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("usage of a root removed during the walk: %v, want not exist", err)
	}
}

// A directory replaced by a file between the listing and the descent, and a
// parent replaced by a file before its child is examined, are not removals:
// the sum fails rather than skipping them. The error is ENOTDIR, which on
// Windows is ERROR_PATH_NOT_FOUND, the same error as a missing path: there the
// sum must tell the two apart by looking, not by the error.
func TestUsageFailsWhenADirectoryBecomesAFile(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		hook string // path, under the root, at which the directory is replaced
	}{
		{"directory replaced before its descent", "sub"},
		{"parent replaced before its child is examined", filepath.Join("sub", "x")},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			sub := filepath.Join(dir, "sub")
			if err := os.Mkdir(sub, 0o700); err != nil {
				t.Fatal(err)
			}
			writeSized(t, filepath.Join(sub, "x"), 4)
			size, err := walk(dir, at(filepath.Join(dir, c.hook), func() {
				if err := os.RemoveAll(sub); err != nil {
					t.Error(err)
				}
				writeSized(t, sub, 9)
			}))
			if err == nil {
				t.Fatalf("usage = %d, nil; want a not a directory error", size)
			}
			// Whether Windows reports a path below a file as ERROR_PATH_NOT_FOUND
			// (which is ENOTDIR there) or ERROR_FILE_NOT_FOUND is not known, so
			// for the parent there it is enough that the sum fails.
			if (!onWindows || c.hook == "sub") && !errors.Is(err, syscall.ENOTDIR) {
				t.Fatalf("usage = %d, %v; want a not a directory error", size, err)
			}
			if !onWindows && !strings.Contains(err.Error(), "not a directory") {
				t.Fatalf("usage error %q does not say not a directory", err)
			}
		})
	}
}

// gone is true for a path that is missing below directories that exist, and
// false when something above it is not a directory, which on Windows reports
// the same not-exist error as a missing path.
func TestGoneTellsAMissingPathFromOneBelowAFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "file")
	writeSized(t, file, 1)
	if err := os.MkdirAll(filepath.Join(dir, "d", "e"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		path string
		want bool
	}{
		{"missing in a directory", filepath.Join(dir, "d", "missing"), true},
		{"missing below a missing directory", filepath.Join(dir, "d", "no", "deeper", "x"), true},
		{"present", filepath.Join(dir, "d", "e"), false},
		{"below a file", filepath.Join(file, "x"), false},
		{"deep below a file", filepath.Join(file, "x", "y"), false},
	} {
		if got := gone(dir, c.path); got != c.want {
			t.Errorf("gone(%s) = %v, want %v", c.name, got, c.want)
		}
	}
}

// A root that is a symbolic link or a regular file is refused rather than
// summed as the one entry it is, which would be a tiny sum for a tree.
func TestUsageRefusesARootThatIsNotADirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tree := filepath.Join(dir, "tree")
	if err := os.Mkdir(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	writeSized(t, filepath.Join(tree, "a"), 1000)
	file := filepath.Join(dir, "file")
	writeSized(t, file, 10)
	roots := map[string]string{"regular file": file}
	if !onWindows {
		link := filepath.Join(dir, "link")
		if err := os.Symlink(tree, link); err != nil {
			t.Fatal(err)
		}
		roots["symbolic link to a directory"] = link
	}
	for name, root := range roots {
		if size, err := Usage(root, nil); err == nil || !strings.Contains(err.Error(), "is not a directory") {
			t.Errorf("Usage of a %s = %d, %v; want an is not a directory error", name, size, err)
		}
	}
}

// Sizes are the entry's own, as lstat reports them: a symbolic link is
// counted at its own length and is never followed, to a file or to a
// directory.
func TestUsageDoesNotFollowSymbolicLinks(t *testing.T) {
	t.Parallel()
	if onWindows {
		t.Skip("creating symbolic links on Windows needs a privilege the test does not assume")
	}
	outside := t.TempDir()
	writeSized(t, filepath.Join(outside, "big"), 100000)
	if err := os.Mkdir(filepath.Join(outside, "tree"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeSized(t, filepath.Join(outside, "tree", "inner"), 50000)
	dir := t.TempDir()
	fileLink := filepath.Join(dir, "file-link")
	dirLink := filepath.Join(dir, "dir-link")
	if err := os.Symlink(filepath.Join(outside, "big"), fileLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "tree"), dirLink); err != nil {
		t.Fatal(err)
	}
	var want int64
	for _, l := range []string{fileLink, dirLink} {
		info, err := os.Lstat(l)
		if err != nil {
			t.Fatal(err)
		}
		want += info.Size()
	}
	size, err := Usage(dir, nil)
	if err != nil || size != want {
		t.Fatalf("Usage = %d, %v; want %d (the links' own lengths), no error", size, err, want)
	}
}
