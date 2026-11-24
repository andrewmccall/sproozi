package main

import (
	"os"
	"testing"
	"time"
)

// signedIn is a login sync whose store and harness file both hold
// refreshedR3, its periodic sync running.
func signedIn(t *testing.T) *loginSync {
	t.Helper()
	return signedInEvery(t, 0)
}

// unreachedSaveWait bounds the syncs of a test about what a sync copies, not
// about its bound. Nothing holds their store's lock, so each sync, and a
// stop's save, ends when its reads and writes do; a stalled machine makes it
// slower, never a skipped save. A test about the bound sets testSaveWait.
const unreachedSaveWait = time.Hour

// signedInEvery is signedIn with its periodic sync every interval.
func signedInEvery(t *testing.T, interval time.Duration) *loginSync {
	t.Helper()
	l := runningWithin(t, interval, unreachedSaveWait)
	write(t, l.native, refreshedR3)
	syncAll(t, l)
	if content(t, l.stored) != refreshedR3 {
		t.Fatal("the sign-in didn't reach the store")
	}
	return l
}

// A login file cut short while the harness rewrote it is not a login: it is
// never copied to the store that every session of the login shares, while
// the harness runs or when it stops.
func TestHalfWrittenLoginIsNeverShared(t *testing.T) {
	for name, half := range map[string]string{
		"cut short":          `{"refresh":"r`,
		"old tail left over": `{"refresh":"r4"}efresh":"r3"}`,
		"not an object":      `"r4"`,
	} {
		t.Run(name, func(t *testing.T) {
			l := signedIn(t)
			write(t, l.native, half)
			if err := l.sync(); err == nil {
				t.Fatal("a half-written login synced without a word")
			}
			if content(t, l.stored) != refreshedR3 {
				t.Fatalf("running: the store holds %q", content(t, l.stored))
			}
			stopWithin(t, l)
			if content(t, l.stored) != refreshedR3 {
				t.Fatalf("stopped: the store holds %q", content(t, l.stored))
			}
		})
	}
}

// Copilot writes config.json with a header of comment lines; that is still
// a whole login.
func TestCommentedCopilotConfigIsShared(t *testing.T) {
	l := signedIn(t)
	commented := "// Copilot settings\n// Do not edit\n{\n  \"logged_in_users\": [{\"login\": \"octocat\"}]\n}\n"
	write(t, l.native, commented)
	syncAll(t, l)
	if content(t, l.stored) != commented {
		t.Fatalf("the store holds %q", content(t, l.stored))
	}
}

func remove(t *testing.T, name string) {
	t.Helper()
	if err := os.Remove(name); err != nil {
		t.Fatal(err)
	}
}

// A harness that empties or removes its login file once a stop has begun may
// have been cut off rewriting it, so the stop never signs the other sessions
// out. A sign-out while the harness runs still reaches them
// (TestTwoSessionsOfOneLoginBothSurviveARefreshByEither).
func TestStopNeverSignsTheLoginOut(t *testing.T) {
	for name, signOut := range map[string]func(*testing.T, *loginSync){
		"emptied": func(t *testing.T, l *loginSync) { write(t, l.native, "") },
		"removed": func(t *testing.T, l *loginSync) { remove(t, l.native) },
	} {
		t.Run(name, func(t *testing.T) {
			l := signedIn(t)
			l.stopping()
			signOut(t, l)
			syncAll(t, l)
			if content(t, l.stored) != refreshedR3 {
				t.Fatalf("stopping: the store holds %q", content(t, l.stored))
			}
			stopWithin(t, l)
			if content(t, l.stored) != refreshedR3 {
				t.Fatalf("stopped: the store holds %q", content(t, l.stored))
			}
		})
	}
}

// A refresh while a stop waits for the turn, before any kill, still reaches
// the store.
func TestRefreshWhileStoppingIsShared(t *testing.T) {
	l := signedIn(t)
	l.stopping()
	write(t, l.native, refreshedR4)
	stopWithin(t, l)
	if content(t, l.stored) != refreshedR4 {
		t.Fatalf("the store holds %q", content(t, l.stored))
	}
}

// After the kill is recorded, nothing in the harness's login file reaches
// the store, not even a whole login, and a change left unsaved is logged.
func TestForcedStopLeavesTheStoreAsItIs(t *testing.T) {
	l := signedIn(t)
	l.forced()
	write(t, l.native, refreshedR4)
	if err := l.sync(); err == nil {
		t.Fatal("a change the stop could not save synced without a word")
	}
	stopWithin(t, l)
	if content(t, l.stored) != refreshedR3 {
		t.Fatalf("the store holds %q", content(t, l.stored))
	}
	missedSave(t, l)
}
