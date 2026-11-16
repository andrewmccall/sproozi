package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
)

// signedIn is a login sync whose store and harness file both hold
// refreshedR3, its periodic sync running.
func signedIn(t *testing.T) *loginSync {
	t.Helper()
	return signedInUntil(t, time.Time{})
}

// signedInUntil is signedIn with a launch deadline.
func signedInUntil(t *testing.T, until time.Time) *loginSync {
	t.Helper()
	l := runningUntil(t, until)
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
	write(t, l.native, `{"refresh":"r4"}`)
	stopWithin(t, l)
	if content(t, l.stored) != `{"refresh":"r4"}` {
		t.Fatalf("the store holds %q", content(t, l.stored))
	}
}

// After the supervisor kills its harness, or once the deadline at which it
// is killed has passed, nothing in the harness's login file reaches the
// store, and a change left unsaved is logged.
func TestForcedStopLeavesTheStoreAsItIs(t *testing.T) {
	for name, force := range map[string]func(*testing.T) *loginSync{
		"killed": func(t *testing.T) *loginSync {
			l := signedIn(t)
			l.forced()
			return l
		},
		"deadline": func(t *testing.T) *loginSync {
			until := time.Now().Add(300 * time.Millisecond)
			l := signedInUntil(t, until)
			time.Sleep(time.Until(until))
			return l
		},
	} {
		t.Run(name, func(t *testing.T) {
			l := force(t)
			write(t, l.native, `{"refresh":"r4"}`)
			if err := l.sync(); err == nil {
				t.Fatal("a change the stop could not save synced without a word")
			}
			stopWithin(t, l)
			if content(t, l.stored) != refreshedR3 {
				t.Fatalf("the store holds %q", content(t, l.stored))
			}
			missedSave(t, l)
		})
	}
}

// Claude Code and Copilot are always stopped by a kill: a login file the
// kill left empty does not sign the login out.
func TestKillingStopLeavesTheStoreAsItIs(t *testing.T) {
	for _, kind := range []harness.NativeKind{harness.NativeClaude, harness.NativeCopilot} {
		t.Run(string(kind), func(t *testing.T) {
			withAdapter(t, kind, func(a *nativeAdapter) {
				a.observe = func(*server) (string, string, error) { return selected, completedTurnID, nil }
			})
			s, _ := stopping(t, kind, ignoresQuit, time.Time{})
			s.login = signedIn(t)
			// The harness was rewriting its login when the kill landed.
			finish := nativeAdapters[kind].finish
			withAdapter(t, kind, func(a *nativeAdapter) {
				a.finish = func(n *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error) {
					exit, err := finish(n, ctx)
					write(t, s.login.native, "")
					return exit, err
				}
			})
			r, _ := stopOnce(t, s)
			if r.State != harness.NativeStopped {
				t.Fatalf("stop: %+v", r)
			}
			if content(t, s.login.stored) != refreshedR3 {
				t.Fatalf("the store holds %q", content(t, s.login.stored))
			}
		})
	}
}
