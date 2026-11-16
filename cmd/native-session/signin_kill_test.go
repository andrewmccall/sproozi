package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
	"golang.org/x/sys/unix"
)

// quietInterval keeps the periodic sync from running in a test, so only the
// final save, or a sync the test runs, copies anything.
const quietInterval = time.Hour

// unsavedLogin is a whole login, different from the stored one, that the
// harness wrote and no sync has copied yet. Only a kill record keeps it out
// of the store: the stop rules let a whole login through.
const unsavedLogin = `{"refresh":"r9"}`

// ignoringTerminal is activate's terminal for a harness that ignores its
// quit, run from the test's shell in a temporary workspace.
func ignoringTerminal(t *testing.T, login *loginSync) nativepty.Launch {
	t.Helper()
	launch := terminalLaunch("/bin/sh", []string{"-c", ignoresQuit},
		[]string{testPathEnvironment, testTerminalEnvironment}, login)
	launch.Dir = t.TempDir()
	return launch
}

// killable is a supervisor of kind whose harness ignores its quit, on
// activate's terminal, which records every kill in the login sync before
// sending it. Only a watched one has its exit watcher, which saves the
// sign-in and records a kill it sees after the fact; without it, only the
// record made before the kill keeps unsavedLogin out of the store. The store
// holds refreshedR3 and the harness's file unsavedLogin.
func killable(t *testing.T, kind harness.NativeKind, until time.Time, watched bool) (*server, *loginSync) {
	t.Helper()
	l := signedInEvery(t, quietInterval)
	native, err := nativepty.Start(ignoringTerminal(t, l))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = native.Stop(ctx)
	})
	s := &server{native: native, life: lifecycle{phase: phaseActive}, threadID: selected, done: make(chan struct{}),
		config: config{Harness: kind, ThreadID: selected, Deadline: until}, deadline: until,
		reports: &bytes.Buffer{}, diagnostics: &bytes.Buffer{}, login: l}
	if watched {
		go s.watch(native, l.stop)
	}
	write(t, l.native, unsavedLogin)
	return s, l
}

// Every way the supervisor or anything else kills the harness is recorded
// before the final save reads its login file, so a whole login it wrote is
// not saved: a kill can land while the harness rewrites the file, and a
// client that rewrites in place without truncating could leave a whole
// login that is wrong.
func TestEveryKillLeavesTheStoreAsItWas(t *testing.T) {
	completed := func(a *nativeAdapter) {
		a.observe = func(*server) (string, string, error) { return selected, completedTurnID, nil }
	}
	stopsGracefully := func(t *testing.T, s *server) {
		t.Helper()
		stopOnce(t, s)
	}
	for name, c := range map[string]struct {
		kind    harness.NativeKind
		until   time.Duration
		watched bool
		before  func(t *testing.T)
		drive   func(t *testing.T, s *server)
	}{
		"claude code's kill": {kind: harness.NativeClaude, before: func(t *testing.T) {
			withAdapter(t, harness.NativeClaude, completed)
		}, drive: stopsGracefully},
		"copilot's kill": {kind: harness.NativeCopilot, before: func(t *testing.T) {
			withAdapter(t, harness.NativeCopilot, completed)
		}, drive: stopsGracefully},
		"codex at the quit bound": {kind: harness.NativeCodex, before: func(t *testing.T) {
			withAdapter(t, harness.NativeCodex, completed)
			setQuitWait(t, 300*time.Millisecond)
		}, drive: stopsGracefully},
		"codex with its input full": {kind: harness.NativeCodex, before: func(t *testing.T) {
			withAdapter(t, harness.NativeCodex, func(a *nativeAdapter) {
				completed(a)
				a.finish = func(*nativepty.Supervisor, context.Context) (nativepty.Exit, error) {
					return nativepty.Exit{}, fmt.Errorf("send native quit: %w",
						errors.Join(nativepty.ErrInputBusy, io.ErrShortWrite))
				}
			})
		}, drive: stopsGracefully},
		"turnless stop": {kind: harness.NativeCodex, before: func(t *testing.T) {
			if codexJournalExists(nativeHome) {
				t.Skip("a real Codex home is present")
			}
			setTurnlessSettle(t, 0)
		}, drive: func(t *testing.T, s *server) {
			s.config.ThreadID, s.threadID = "", ""
			stopsGracefully(t, s)
		}},
		"terminated": {kind: harness.NativeCodex, drive: func(t *testing.T, s *server) {
			ended(t, s, func() { s.stop(evTerminate) })
		}},
		"deadline": {kind: harness.NativeCodex, until: 500 * time.Millisecond, drive: func(t *testing.T, s *server) {
			ended(t, s, func() { go s.watchDeadline() })
		}},
		"signal from outside": {kind: harness.NativeCodex, watched: true, drive: func(t *testing.T, s *server) {
			ended(t, s, func() { _ = unix.Kill(-s.native.PID(), unix.SIGKILL) })
		}},
	} {
		t.Run(name, func(t *testing.T) {
			if c.before != nil {
				c.before(t)
			}
			until := time.Time{}
			if c.until > 0 {
				until = time.Now().Add(c.until)
			}
			s, l := killable(t, c.kind, until, c.watched)
			c.drive(t, s)
			if got := content(t, l.stored); got != refreshedR3 {
				t.Fatalf("the store holds %q after the kill", got)
			}
		})
	}
}

// Gating input for a stop is recorded too: a sign-out two syncs see after it
// is not shared, because the coming kill may cut a rewrite short.
func TestGatedInputStopsSharingASignOut(t *testing.T) {
	s, l := killable(t, harness.NativeCodex, time.Time{}, false)
	s.gate()
	write(t, l.native, "")
	syncAll(t, l, l)
	if got := content(t, l.stored); got != refreshedR3 {
		t.Fatalf("the store holds %q", got)
	}
}

// After a kill, a sync whose file did not change does not open the store at
// all: a stop whose harness was killed never waits for a locked store.
func TestUnchangedLoginAfterAKillLeavesTheStoreAlone(t *testing.T) {
	l := signedIn(t)
	l.forced()
	lockedByPeer(t, l.stored)
	stopWithin(t, l)
	if said := l.diagnostics.(*lockedBuffer).String(); said != "" {
		t.Fatalf("diagnostics: %q", said)
	}
}

// activate's terminal records a kill in the login sync before sending it.
func TestTheHarnessTerminalRecordsItsKills(t *testing.T) {
	l := signedIn(t)
	native, err := nativepty.Start(ignoringTerminal(t, l))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err = native.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if l.trust() != loginForced {
		t.Fatal("the kill was not recorded")
	}
}

// A login file left half written before this launch is replaced by the
// store's login, or removed when the store has none; it never reaches the
// store.
func TestFirstSyncReplacesAHalfWrittenLogin(t *testing.T) {
	for name, stored := range map[string]string{"signed in": refreshedR3, "signed out": ""} {
		t.Run(name, func(t *testing.T) {
			store := t.TempDir()
			write(t, filepath.Join(store, "auth.json"), stored)
			l := &loginSync{stored: filepath.Join(store, "auth.json"), native: filepath.Join(t.TempDir(), "auth.json"),
				interval: quietInterval, quit: make(chan struct{}), stopped: make(chan struct{}),
				diagnostics: &lockedBuffer{}, wait: testSaveWait}
			write(t, l.native, `{"refresh":"r`)
			if err := l.begin(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(l.stop)
			want := stored
			if want == "" {
				want = missing
			}
			if content(t, l.native) != want || content(t, l.stored) != stored {
				t.Fatalf("harness %q, store %q", content(t, l.native), content(t, l.stored))
			}
		})
	}
}
