package main

import (
	"testing"
)

// signOuts are the ways a login file says "not signed in": empty, missing,
// or an object with nothing in it.
var signOuts = map[string]func(*testing.T, string){
	"emptied":      func(t *testing.T, name string) { write(t, name, "") },
	"removed":      func(t *testing.T, name string) { remove(t, name) },
	"empty object": func(t *testing.T, name string) { write(t, name, "{ }\n") },
}

// Codex and Claude Code rewrite their login by truncating it first, so one
// sync can catch it empty while the harness runs normally. A single empty
// read shares nothing; a sign-out still there at the next sync is shared.
// The test's own syncs are the only ones: a periodic sync between them would
// be a second that saw it.
func TestSignOutIsSharedOnlyWhenTwoSyncsSeeIt(t *testing.T) {
	for name, signOut := range signOuts {
		t.Run(name, func(t *testing.T) {
			l := signedInEvery(t, quietInterval)
			signOut(t, l.native)
			syncAll(t, l)
			if content(t, l.stored) != refreshedR3 {
				t.Fatalf("one empty read was shared: the store holds %q", content(t, l.stored))
			}
			write(t, l.native, refreshedR3) // The rewrite finished.
			syncAll(t, l)
			signOut(t, l.native)
			syncAll(t, l)
			if content(t, l.stored) != refreshedR3 {
				t.Fatalf("an empty read after a whole one was shared: %q", content(t, l.stored))
			}
			syncAll(t, l)
			if content(t, l.stored) != "" {
				t.Fatalf("a sign-out two syncs saw was not shared: the store holds %q", content(t, l.stored))
			}
		})
	}
}

// The store side has the same rule: a store read empty once, as one another
// writer is rewriting, does not sign the harness out; one still empty at the
// next sync does. Again the test's syncs are the only ones.
func TestEmptyStoreSignsTheHarnessOutOnlyWhenTwoSyncsSeeIt(t *testing.T) {
	for name, signOut := range signOuts {
		t.Run(name, func(t *testing.T) {
			l := signedInEvery(t, quietInterval)
			signOut(t, l.stored)
			syncAll(t, l)
			if content(t, l.native) != refreshedR3 {
				t.Fatalf("one empty store read signed the harness out: %q", content(t, l.native))
			}
			syncAll(t, l)
			if content(t, l.native) != missing {
				t.Fatalf("a sign-out two syncs saw did not reach the harness: %q", content(t, l.native))
			}
		})
	}
}

// An object with nothing in it is a sign-out, so it is not shared once a stop
// has begun, as an empty file is not.
func TestEmptyObjectIsNotSharedFromAStop(t *testing.T) {
	l := signedIn(t)
	l.stopping()
	write(t, l.native, "{}")
	syncAll(t, l)
	syncAll(t, l)
	stopWithin(t, l)
	if content(t, l.stored) != refreshedR3 {
		t.Fatalf("the store holds %q", content(t, l.stored))
	}
}
