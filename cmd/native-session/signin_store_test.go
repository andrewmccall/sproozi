package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// errStalled is a store write that never finished: the volume stalled, or
// the supervisor exited while the write was in flight.
var errStalled = errors.New("the write did not finish")

// stallingStore is a store file whose writes stop part way: after budget
// units, counting each byte written and each truncate or sync as one, every
// operation fails.
type stallingStore struct {
	content []byte
	budget  int
}

func (f *stallingStore) spend() bool {
	if f.budget <= 0 {
		return false
	}
	f.budget--
	return true
}

func (f *stallingStore) WriteAt(data []byte, offset int64) (int, error) {
	for i, b := range data {
		if !f.spend() {
			return i, errStalled
		}
		at := int(offset) + i
		for len(f.content) <= at {
			f.content = append(f.content, 0)
		}
		f.content[at] = b
	}
	return len(data), nil
}

func (f *stallingStore) Truncate(size int64) error {
	if !f.spend() {
		return errStalled
	}
	for int64(len(f.content)) < size {
		f.content = append(f.content, 0)
	}
	f.content = f.content[:size]
	return nil
}

func (f *stallingStore) Sync() error {
	if !f.spend() {
		return errStalled
	}
	return nil
}

// A store write that stops at any point leaves the old login, the new one,
// or content no reader acts on: never an empty store, which every other
// session would take for a sign-out, and never a whole login that is
// neither, as a longer login written part way over a shorter one could be.
func TestStoreWriteNeverStopsInAStateAReaderActsOn(t *testing.T) {
	copilotOld := "// Copilot settings\n{\"logged_in_users\":[\"a\"]}\n"
	copilotNew := "// Copilot settings\n{\"logged_in_users\":[\"octocat\"],\"trustedFolders\":[\"/w\"]}\n"
	for name, pair := range map[string][2]string{
		"longer":        {`{"t":"aaaa"}`, `{"t":"bbbbbbbb"}`},
		"shorter":       {`{"t":"bbbbbbbb"}`, `{"t":"aaaa"}`},
		"first sign-in": {"", refreshedR3},
		"copilot":       {copilotOld, copilotNew},
	} {
		t.Run(name, func(t *testing.T) {
			old, next := []byte(pair[0]), []byte(pair[1])
			for budget := 0; ; budget++ {
				f := &stallingStore{content: bytes.Clone(old), budget: budget}
				err := writeStore(context.Background(), f, next)
				got := f.content
				acted := nonEmpty(got) == nil || wholeLogin(got)
				if acted && !bytes.Equal(got, old) && !bytes.Equal(got, next) {
					t.Fatalf("stopped after %d units in a state a reader acts on: %q", budget, got)
				}
				if nonEmpty(got) == nil && len(old) > 0 {
					t.Fatalf("stopped after %d units with the store empty", budget)
				}
				if err == nil {
					if !bytes.Equal(got, next) {
						t.Fatalf("a finished write left %q", got)
					}
					return
				}
			}
		})
	}
}

// A store another writer left part way through is not a login: a running
// session keeps its own login rather than copying it in, and the next change
// it makes replaces the unfinished store.
func TestUnfinishedStoreIsNeverCopiedIntoTheHarness(t *testing.T) {
	for name, unfinished := range map[string]string{
		"cut short": `{"refresh":"r`,
		"marked":    "#\"refresh\":\"r4\"}",
	} {
		t.Run(name, func(t *testing.T) {
			l := signedIn(t)
			write(t, l.stored, unfinished)
			if err := l.sync(); err == nil {
				t.Fatal("an unfinished store synced without a word")
			}
			if content(t, l.native) != refreshedR3 {
				t.Fatalf("the harness's login became %q", content(t, l.native))
			}
			write(t, l.native, `{"refresh":"r4"}`)
			syncAll(t, l)
			if content(t, l.stored) != `{"refresh":"r4"}` {
				t.Fatalf("the next change left the store %q", content(t, l.stored))
			}
		})
	}
}

// A session that starts while the store is unfinished starts without a
// login, rather than failing to start or taking the unfinished bytes.
func TestStartWithAnUnfinishedStoreStartsWithoutALogin(t *testing.T) {
	store := t.TempDir()
	write(t, filepath.Join(store, "auth.json"), `{"refresh":"r`)
	l := &loginSync{stored: filepath.Join(store, "auth.json"), native: filepath.Join(t.TempDir(), "auth.json"),
		quit: make(chan struct{}), stopped: make(chan struct{}), diagnostics: &lockedBuffer{}, wait: testSaveWait}
	if err := l.begin(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.stop)
	if content(t, l.native) != missing {
		t.Fatalf("the harness started with %q", content(t, l.native))
	}
	if !strings.Contains(l.diagnostics.(*lockedBuffer).String(), "not a whole login") {
		t.Fatalf("diagnostics: %q", l.diagnostics.(*lockedBuffer).String())
	}
}

// Once a save's bound has passed nothing is written: not a free lock taken
// late, and not a write begun after it.
func TestNothingIsWrittenOnceTheSaveGaveUp(t *testing.T) {
	stored := filepath.Join(t.TempDir(), "auth.json")
	write(t, stored, refreshedR3)
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	f, err := os.OpenFile(stored, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	if err = lockStore(ended, f, syscall.LOCK_EX); err == nil {
		t.Fatal("a save whose bound had passed took the lock")
	}
	peer, err := os.OpenFile(stored, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Close() }()
	if err = syscall.Flock(int(peer.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("the lock stayed taken: %v", err)
	}
	if err = writeStore(ended, f, []byte(`{"refresh":"r4"}`)); err == nil {
		t.Fatal("a save whose bound had passed wrote")
	}
	if content(t, stored) != refreshedR3 {
		t.Fatalf("the store holds %q", content(t, stored))
	}
}
