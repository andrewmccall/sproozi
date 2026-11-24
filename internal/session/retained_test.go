//go:build !windows

package session

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/andrewmccall/sproozi/internal/testutil"
)

func openRetained(t *testing.T) *Coordinator {
	t.Helper()
	c, err := Open(filepath.Join(testutil.PrivateDir(t), "journal"),
		map[string]Placement{testLocal: newPlacement(nil, "")}, testMaxCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func retainedSize(t *testing.T, c *Coordinator) int64 {
	t.Helper()
	_, size, err := c.Retained()
	if err != nil {
		t.Fatal(err)
	}
	return size
}

// A replacement's temporary file counts as kept while it exists, beside the
// file it replaces: the sum may count a journal twice for a moment, which
// only refuses work sooner.
func TestRetainedCountsAReplacementInProgress(t *testing.T) {
	t.Parallel()
	c := openRetained(t)
	base := retainedSize(t, c)
	if err := os.WriteFile(filepath.Join(c.dir, checkpointsDir, ".pending-0123456789abcdef0123456789abcdef"), make([]byte, 11), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := retainedSize(t, c); got != base+11 {
		t.Fatalf("Retained = %d with an 11-byte replacement in progress, want %d", got, base+11)
	}
}

// The walk that sums what is kept examines nothing but sizes, as it always
// has: a stray file, a FIFO, a symbolic link and a record nobody can read
// count at their own size, and a directory it cannot list fails the sum.
func TestRetainedKeepsItsRefusals(t *testing.T) {
	t.Parallel()
	c := openRetained(t)
	base := retainedSize(t, c)
	const target = "/nonexistent/link/target"
	if err := os.WriteFile(filepath.Join(c.dir, "stray"), make([]byte, 7), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(c.dir, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(c.dir, workspacesDir, "link")); err != nil {
		t.Fatal(err)
	}
	record := filepath.Join(c.dir, "6f1c7d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f.json")
	if err := os.WriteFile(record, make([]byte, 5), 0o000); err != nil {
		t.Fatal(err)
	}
	executions, size, err := c.Retained()
	if err != nil || executions != 1 || size != base+7+int64(len(target))+5 {
		t.Fatalf("Retained = %d, %d, %v; want 1, %d, no error", executions, size, err, base+7+int64(len(target))+5)
	}

	unlistable := filepath.Join(c.dir, checkpointsDir)
	if err = os.Chmod(unlistable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unlistable, 0o700) })
	_, _, err = c.Retained()
	if want := "open " + unlistable + ": permission denied"; err == nil || err.Error() != want {
		t.Fatalf("Retained with an unlistable directory: %v, want %q", err, want)
	}
}
