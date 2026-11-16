package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
)

// lockedByPeer holds the store's exclusive lock, as another session's
// supervisor on a stalled volume would, until the test ends.
func lockedByPeer(t *testing.T, stored string) {
	t.Helper()
	f, err := os.OpenFile(stored, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
}

// testSaveWait bounds a test's login syncs' waits for their stores.
const testSaveWait = 200 * time.Millisecond

// lockedBuffer keeps the diagnostics of syncs that run in the background.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// running is a login sync between a fresh store and harness file whose
// periodic sync runs, as activate leaves it, with its diagnostics kept and
// testSaveWait's bound. It is stopped when the test ends.
func running(t *testing.T) *loginSync {
	t.Helper()
	return runningUntil(t, time.Time{})
}

// runningUntil is running with a launch deadline.
func runningUntil(t *testing.T, until time.Time) *loginSync {
	t.Helper()
	l := &loginSync{stored: filepath.Join(t.TempDir(), "auth.json"), native: filepath.Join(t.TempDir(), "auth.json"),
		deadline: until, quit: make(chan struct{}), stopped: make(chan struct{}), diagnostics: &lockedBuffer{},
		wait: testSaveWait}
	if err := l.sync(); err != nil {
		t.Fatal(err)
	}
	go l.run()
	t.Cleanup(func() {
		l.stop()
		<-l.stopped
	})
	return l
}

// stopLimit is how long a test lets a login sync's stop take before failing
// rather than hanging the suite.
const stopLimit = 3 * time.Second

// stopWithin runs stop and fails the test if it has not returned within
// stopLimit.
func stopWithin(t *testing.T, l *loginSync) {
	t.Helper()
	done := make(chan struct{})
	go func() { l.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(stopLimit):
		t.Fatalf("saving the sign-in did not return within %s", stopLimit)
	}
}

// missedSave checks that the save l skipped was logged once, in the house
// style.
func missedSave(t *testing.T, l *loginSync) {
	t.Helper()
	said := l.diagnostics.(*lockedBuffer).String()
	skipped := strings.Count(said, "Skipped saving the harness's sign-in")
	if skipped != 1 || strings.HasSuffix(strings.TrimSpace(said), ".") {
		t.Fatalf("diagnostics: %q", said)
	}
}

// A peer holding the store's lock, as on a stalled volume, never holds a
// stop: the save gives up at its bound, the store keeps what it had, and the
// missed save is logged.
func TestStopDoesNotWaitForALockedLoginStore(t *testing.T) {
	l := running(t)
	write(t, l.native, refreshedR3)
	lockedByPeer(t, l.stored)
	stopWithin(t, l)
	if content(t, l.stored) != "" {
		t.Fatalf("a locked store was written: %q", content(t, l.stored))
	}
	missedSave(t, l)
}

// The first sync, under activate's action lock, has the same bound.
func TestActivationDoesNotWaitForALockedLoginStore(t *testing.T) {
	store := t.TempDir()
	write(t, filepath.Join(store, "auth.json"), refreshedR3)
	lockedByPeer(t, filepath.Join(store, "auth.json"))
	l := &loginSync{stored: filepath.Join(store, "auth.json"), native: filepath.Join(t.TempDir(), "auth.json"),
		wait: testSaveWait}
	if err := l.sync(); err == nil || !strings.Contains(err.Error(), "locked") {
		t.Fatalf("a sync of a locked store: %v", err)
	}
}

// A stop completes although the login store stays locked: the harness is
// stopped and the checkpoint matters more than the save. Codex here quits
// when asked, so its refreshed login is one the stop would save.
func TestStopCompletesWhileTheLoginStoreIsLocked(t *testing.T) {
	completedCodexTurn(t)
	s, _ := stopping(t, harness.NativeCodex, "read -r line; exit 0", time.Time{})
	s.login = running(t)
	write(t, s.login.native, refreshedR3)
	lockedByPeer(t, s.login.stored)
	r, reports := stopOnce(t, s)
	if r.Error != "" || r.State != harness.NativeStopped || r.Exit == nil ||
		r.Exit.Reason != string(nativepty.NativeExit) || len(reports) != 0 {
		t.Fatalf("stop: %+v exit %+v reports %+v", r, r.Exit, reports)
	}
	missedSave(t, s.login)
}
