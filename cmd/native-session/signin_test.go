package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/harness"
)

func TestSignInPathsComeFromTheRegistry(t *testing.T) {
	for kind, want := range map[harness.NativeKind]string{
		harness.NativeClaude:  "/workspace/session/claude/.credentials.json",
		harness.NativeCodex:   "/workspace/session/codex/auth.json",
		harness.NativeCopilot: "/workspace/session/copilot/config.json",
	} {
		stored, native, ok := signInPaths(config{Harness: kind, SignInDirectory: api.SignInDirectory})
		if !ok || native != want || filepath.Dir(stored) != api.SignInDirectory {
			t.Fatalf("%s: stored=%s native=%s ok=%v", kind, stored, native, ok)
		}
	}
	if _, _, ok := signInPaths(config{Harness: harness.NativeClaude}); ok {
		t.Fatal("a launch without a sign-in directory restored a sign-in")
	}
}

// session is one supervisor's view: its harness's login file and the shared
// store.
func session(t *testing.T, store string) *loginSync {
	t.Helper()
	l := &loginSync{stored: filepath.Join(store, "auth.json"), native: filepath.Join(t.TempDir(), "auth.json")}
	if err := l.sync(); err != nil {
		t.Fatal(err)
	}
	return l
}

func write(t *testing.T, name, data string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func content(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return "<missing>"
	}
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func syncAll(t *testing.T, sessions ...*loginSync) {
	t.Helper()
	for _, l := range sessions {
		if err := l.sync(); err != nil {
			t.Fatal(err)
		}
	}
}

// Two sessions of one login share a live store (ADR 0049): a sign-in, a
// refresh (which rotates the refresh token) or a sign-out in either reaches
// the other, as it would between two processes on one machine.
func TestTwoSessionsOfOneLoginBothSurviveARefreshByEither(t *testing.T) {
	store := t.TempDir()
	// Docker's init-login leaves an empty file for the native user.
	write(t, filepath.Join(store, "auth.json"), "")
	a, b := session(t, store), session(t, store)
	if content(t, a.native) != "<missing>" || content(t, b.native) != "<missing>" {
		t.Fatal("an empty store produced a sign-in")
	}
	write(t, a.native, `{"refresh":"r1"}`) // A signs in.
	syncAll(t, a, b)
	if got := content(t, b.native); got != `{"refresh":"r1"}` {
		t.Fatalf("B didn't get A's sign-in: %s", got)
	}
	write(t, b.native, `{"refresh":"r2"}`) // B refreshes; r1 is now spent.
	syncAll(t, b, a)
	if got := content(t, a.native); got != `{"refresh":"r2"}` {
		t.Fatalf("A kept the spent refresh token: %s", got)
	}
	write(t, a.native, `{"refresh":"r3"}`) // A refreshes with r2.
	syncAll(t, a, b)
	if content(t, b.native) != `{"refresh":"r3"}` || content(t, filepath.Join(store, "auth.json")) != `{"refresh":"r3"}` {
		t.Fatal("A's refresh didn't reach B and the store")
	}
	// A third session started now gets the current sign-in.
	if c := session(t, store); content(t, c.native) != `{"refresh":"r3"}` {
		t.Fatal("a new session didn't get the current sign-in")
	}
	if err := os.Remove(b.native); err != nil { // B signs out.
		t.Fatal(err)
	}
	syncAll(t, b, a)
	if content(t, a.native) != "<missing>" {
		t.Fatal("B's sign-out didn't reach A")
	}
}

// Two different logins never touch each other.
func TestDifferentLoginsKeepSeparateStores(t *testing.T) {
	work, personal := session(t, t.TempDir()), session(t, t.TempDir())
	write(t, work.native, `{"account":"work"}`)
	syncAll(t, work, personal)
	if content(t, personal.native) != "<missing>" || content(t, personal.stored) != "" {
		t.Fatal("one login's sign-in reached another")
	}
}

// A stored sign-in the supervisor can't write, such as a Secret's
// non-refreshing token, still reaches the harness and is never overwritten.
func TestReadOnlyStoreOnlyFlowsIn(t *testing.T) {
	store := t.TempDir()
	stored := filepath.Join(store, "auth.json")
	write(t, stored, `{"token":"long-lived"}`)
	if err := os.Chmod(stored, 0o400); err != nil {
		t.Fatal(err)
	}
	l := session(t, store)
	if content(t, l.native) != `{"token":"long-lived"}` {
		t.Fatal("the read-only sign-in didn't reach the harness")
	}
	write(t, l.native, `{"token":"changed"}`)
	syncAll(t, l)
	if content(t, stored) != `{"token":"long-lived"}` {
		t.Fatal("a read-only store was written")
	}
}

// A Secret's key is a link inside its directory, which is followed; a link
// leading out of the sign-in directory, or a file over the limit, is refused.
func TestStoreLinksStayInsideAndFilesStayBounded(t *testing.T) {
	store := t.TempDir()
	write(t, filepath.Join(store, "data"), `{"token":"x"}`)
	if err := os.Symlink("data", filepath.Join(store, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if l := session(t, store); content(t, l.native) != `{"token":"x"}` {
		t.Fatal("a link inside the directory wasn't followed")
	}
	outside := t.TempDir()
	write(t, filepath.Join(outside, "auth.json"), `{"token":"y"}`)
	escape := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "auth.json"), filepath.Join(escape, "auth.json")); err != nil {
		t.Fatal(err)
	}
	l := &loginSync{stored: filepath.Join(escape, "auth.json"), native: filepath.Join(t.TempDir(), "auth.json")}
	if err := l.sync(); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("a link out of the sign-in directory: %v", err)
	}
	big := t.TempDir()
	write(t, filepath.Join(big, "auth.json"), strings.Repeat("x", maxSignInBytes+1))
	l = &loginSync{stored: filepath.Join(big, "auth.json"), native: filepath.Join(t.TempDir(), "auth.json")}
	if err := l.sync(); err == nil {
		t.Fatal("an oversized stored sign-in was accepted")
	}
}

// stop saves the harness's last change once, and a nil sync does nothing.
func TestStopSavesTheLastChange(t *testing.T) {
	store := t.TempDir()
	l, err := startLoginSync(config{Harness: harness.NativeCodex})
	if err != nil || l != nil {
		t.Fatalf("a launch without a sign-in directory: %v %v", l, err)
	}
	l.stop()
	l = &loginSync{stored: filepath.Join(store, "auth.json"), native: filepath.Join(t.TempDir(), "auth.json"),
		quit: make(chan struct{}), stopped: make(chan struct{})}
	if err = l.sync(); err != nil {
		t.Fatal(err)
	}
	go l.run()
	write(t, l.native, `{"refresh":"last"}`)
	l.stop()
	l.stop()
	if content(t, l.stored) != `{"refresh":"last"}` {
		t.Fatal("the last change wasn't saved")
	}
}
