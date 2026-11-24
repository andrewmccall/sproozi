//go:build !windows

package work

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// admitsExactly checks that a new session is admitted with room for exactly
// kept bytes and its two reservations, and refused with one byte less.
func admitsExactly(t *testing.T, o *Owner, kept int64) {
	t.Helper()
	o.config.RetainedBytes = kept + 2*o.config.MaxCheckpointBytes
	if err := o.admitRetention(true); err != nil {
		t.Fatalf("admission with room for %d kept bytes: %v", kept, err)
	}
	o.config.RetainedBytes--
	if err := o.admitRetention(true); code(err) != wireRetentionFull {
		t.Fatalf("admission with room for %d kept bytes less one: %v", kept, err)
	}
}

// A replacement's temporary file counts as kept while it exists, which only
// refuses work sooner.
func TestAdmissionCountsAReplacementInProgress(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	o := openOwner(t, c)
	kept := keptBytes(t, c)
	admitsExactly(t, o, kept)
	if err := os.WriteFile(filepath.Join(c.Directory, ".pending-0123456789abcdef0123456789abcdef"), make([]byte, 11), 0o600); err != nil {
		t.Fatal(err)
	}
	admitsExactly(t, o, kept+11)
}

// Admission's walk of the catalogue examines nothing but sizes, as it always
// has: a stray file, a FIFO, a symbolic link and a record that is not valid
// count at their own size (a record that cannot be read fails admission: see
// TestAdmissionFailsClosedForARecordItCannotRead), and a directory it cannot list fails admission.
func TestAdmissionKeepsItsRefusals(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	o := openOwner(t, c)
	kept := keptBytes(t, c)
	const target = "/nonexistent/link/target"
	if err := os.WriteFile(filepath.Join(c.Directory, "stray"), make([]byte, 7), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(c.Directory, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(c.Directory, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.Directory, "6f1c7d2e-3a4b-4c5d-8e9f-0a1b2c3d4e5f.json"), make([]byte, 5), 0o600); err != nil {
		t.Fatal(err)
	}
	admitsExactly(t, o, kept+7+int64(len(target))+5)

	unlistable := filepath.Join(c.Directory, "unlistable")
	if err := os.Mkdir(unlistable, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(unlistable, 0o700) })
	o.config.RetainedBytes = 1 << 40
	err := o.admitRetention(true)
	if want := "open " + unlistable + ": permission denied"; err == nil || err.Error() != want {
		t.Fatalf("admission with an unlistable directory: %v, want %q", err, want)
	}
}
