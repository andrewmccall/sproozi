package main

import (
	"context"
	"strings"
	"testing"
)

// A sync that keeps refusing the harness's file, as for a client whose login
// format changed, is logged once with its reason, not every second, and once
// more when syncing resumes.
func TestRefusedSyncIsLoggedOncePerChange(t *testing.T) {
	l := signedInEvery(t, quietInterval)
	write(t, l.native, `{"refresh":"r`)
	for range 3 {
		l.tick(context.Background())
	}
	write(t, l.native, refreshedR4)
	for range 2 {
		l.tick(context.Background())
	}
	said := l.diagnostics.(*lockedBuffer).String()
	lines := strings.Split(strings.TrimSpace(said), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "Stopped syncing") ||
		!strings.Contains(lines[0], "not a whole login") || !strings.Contains(lines[1], "Resumed syncing") {
		t.Fatalf("diagnostics: %q", said)
	}
	if content(t, l.stored) != refreshedR4 {
		t.Fatalf("the store holds %q", content(t, l.stored))
	}
}

// A periodic sync that a stop ends while it waits for the store's lock is not
// a failure of syncing: the stop's own save follows and reports itself.
func TestStopEndingAPeriodicSyncIsNotLogged(t *testing.T) {
	l := signedInEvery(t, quietInterval)
	lockedByPeer(t, l.stored)
	ended, end := context.WithCancel(context.Background())
	end()
	l.tick(ended)
	if said := l.diagnostics.(*lockedBuffer).String(); said != "" {
		t.Fatalf("diagnostics: %q", said)
	}
}
