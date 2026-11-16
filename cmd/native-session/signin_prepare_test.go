package main

import (
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// syncsEvery is the periodic interval the start-up tests use, short enough
// that a slow preparation spans several ticks.
const syncsEvery = 20 * time.Millisecond

// startedAgainst is a Copilot session's login sync after its first sync
// against a store holding stored, its periodic sync every syncsEvery, as
// activate leaves it before preparing the launch.
func startedAgainst(t *testing.T, stored string) *loginSync {
	t.Helper()
	store := t.TempDir()
	write(t, filepath.Join(store, "config.json"), stored)
	l := &loginSync{stored: filepath.Join(store, "config.json"), native: filepath.Join(t.TempDir(), "config.json"),
		interval: syncsEvery, quit: make(chan struct{}), stopped: make(chan struct{}),
		diagnostics: &lockedBuffer{}, wait: testSaveWait}
	if err := l.begin(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.stop)
	return l
}

// trusting is Copilot's launch preparation as it touches the login: it
// merges workspace trust into config.json, then takes slow to finish.
func trusting(l *loginSync, slow time.Duration) nativeAdapter {
	return nativeAdapter{prepare: func(_ config, _ string, env []string) ([]string, []string, error) {
		if err := trustCopilotWorkspace(l.native); err != nil {
			return nil, nil, err
		}
		time.Sleep(slow)
		return nil, env, nil
	}}
}

// The launch preparation's write to the login file, however long the
// preparation takes, never reaches a store that held no whole login when the
// session started: the periodic sync starts only once preparation is done,
// and then takes the prepared file as its starting point.
func TestLaunchPreparationNeverReachesAnUnfinishedStore(t *testing.T) {
	for name, slow := range map[string]time.Duration{"quick": 0, "slower than several syncs": 10 * syncsEvery} {
		t.Run(name, func(t *testing.T) {
			l := startedAgainst(t, markedStore)
			if _, _, err := prepareLaunch(trusting(l, slow), config{}, "", nil, l); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if err := l.sync(); !errors.Is(err, errStoreNotWhole) {
					t.Fatalf("a sync of the unfinished store: %v", err)
				}
			}
			if got := content(t, l.stored); got != markedStore {
				t.Fatalf("the launch preparation's write reached the store: %q", got)
			}
		})
	}
}

// A session whose preparation writes nothing to its login still has its
// periodic sync running once it is prepared: a sign-in reaches the store.
func TestPreparedSessionKeepsItsLoginInStep(t *testing.T) {
	l := startedAgainst(t, "")
	plain := nativeAdapter{prepare: func(_ config, _ string, env []string) ([]string, []string, error) {
		return nil, env, nil
	}}
	if _, _, err := prepareLaunch(plain, config{}, "", nil, l); err != nil {
		t.Fatal(err)
	}
	write(t, l.native, refreshedR3)
	for until := time.Now().Add(5 * time.Second); content(t, l.stored) != refreshedR3; time.Sleep(syncsEvery) {
		if time.Now().After(until) {
			t.Fatalf("no periodic sync shared the sign-in: the store holds %q", content(t, l.stored))
		}
	}
}
