package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// liveLogin is a login sync as activate leaves it once the launch is
// prepared: first sync done, periodic sync running at its production
// interval, against a store nobody else holds. Its save bound is one no
// ending reaches, so an ending that waited for its bound would never end.
func liveLogin(t *testing.T) *loginSync {
	t.Helper()
	l := &loginSync{stored: filepath.Join(t.TempDir(), "auth.json"), native: filepath.Join(t.TempDir(), "auth.json"),
		quit: make(chan struct{}), stopped: make(chan struct{}), diagnostics: &lockedBuffer{}, wait: unreachedSaveWait}
	write(t, l.stored, refreshedR3)
	if err := l.sync(); err != nil {
		t.Fatal(err)
	}
	l.settled()
	return l
}

// An ordinary ending, against an uncontended store, ends when its login save
// does, not at the save's bound: from the harness's exit to the stop's reply,
// or to the supervisor concluding after the harness exited by itself,
// whether or not there is a change to save. The bound is out of reach, so
// an ending that happens at all ended with its save, not at the bound; how
// long the save's writes take on a loaded machine does not decide it.
func TestOrdinaryEndingsDoNotWaitForTheLoginBound(t *testing.T) {
	quits := "read -r line; exit 0"
	for _, changed := range []bool{false, true} {
		for name, ending := range map[string]func(t *testing.T, l *loginSync) (exited, ended time.Time){
			"codex quits when asked": func(t *testing.T, l *loginSync) (time.Time, time.Time) {
				completedCodexTurn(t)
				s, _ := stopping(t, harness.NativeCodex, quits, time.Time{})
				s.login = l
				return stopTimed(t, s)
			},
			"claude code is killed": func(t *testing.T, l *loginSync) (time.Time, time.Time) {
				withAdapter(t, harness.NativeClaude, func(a *nativeAdapter) {
					a.observe = func(*server) (string, string, error) { return selected, completedTurnID, nil }
				})
				s, _ := stopping(t, harness.NativeClaude, ignoresQuit, time.Time{})
				s.login = l
				return stopTimed(t, s)
			},
			"the harness exits by itself": func(t *testing.T, l *loginSync) (time.Time, time.Time) {
				native := startNative(t, "sleep 0.3; exit 0", time.Time{})
				s := &server{native: native, life: lifecycle{phase: phaseActive}, threadID: selected, done: make(chan struct{}),
					config: config{Harness: harness.NativeCodex, ThreadID: selected}, reports: &bytes.Buffer{},
					login: l}
				go s.watch(native, l.stop)
				<-native.Done()
				exited := time.Now()
				select {
				case <-s.done:
				case <-time.After(10 * time.Second):
					t.Fatal("the supervisor did not conclude")
				}
				return exited, time.Now()
			},
		} {
			t.Run(name+map[bool]string{false: "", true: ", with a change to save"}[changed], func(t *testing.T) {
				l := liveLogin(t)
				if changed {
					write(t, l.native, `{"refresh":"r5"}`)
				}
				exited, ended := ending(t, l)
				t.Logf("login part of the ending took %s", ended.Sub(exited))
				if said := l.diagnostics.(*lockedBuffer).String(); strings.Contains(said, "unavailable for") {
					t.Fatalf("the ending waited for the save's bound: %q", said)
				}
			})
		}
	}
}

// stopTimed quiesces s and returns when its harness exited and when the stop
// replied.
func stopTimed(t *testing.T, s *server) (time.Time, time.Time) {
	t.Helper()
	exitedAt := make(chan time.Time, 1)
	go func() {
		<-s.native.Done()
		exitedAt <- time.Now()
	}()
	r, _ := stopOnce(t, s)
	replied := time.Now()
	if r.State != harness.NativeStopped {
		t.Fatalf("stop: %+v", r)
	}
	return <-exitedAt, replied
}
