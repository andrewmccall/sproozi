package home

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// A sweep that cannot list the catalogue logs that once, and once when it
// can again. An attach whose exit check fails logs the failure rather than
// discarding it, and still attaches to the running agent.
func TestExitWatcherErrorsAreLogged(t *testing.T) {
	t.Parallel()
	s, local, _, repo := testService(t)
	var log bytes.Buffer
	s.log = &log
	id := startedOn(t, s, repo, testLocal)
	dir, err := s.catalogDir()
	if err != nil {
		t.Fatal(err)
	}
	sweep := func() {
		t.Helper()
		select {
		case <-s.sweepExits(t.Context()):
		case <-time.After(5 * time.Second):
			t.Fatal("sweep did not finish")
		}
	}
	moved := dir + ".moved"
	if err = os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(dir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	sweep()
	sweep()
	if err = os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(moved, dir); err != nil {
		t.Fatal(err)
	}
	sweep()
	local.exitedErr = errors.New("executor unreachable")
	if r := exchange(t, s, Request{Action: ActionAttach, ID: id}); r.Error != "" {
		t.Fatalf("attach replied %+v", r)
	}
	got := log.String()
	if strings.Count(got, "Failed to list sessions to check for exited agents: ") != 1 ||
		strings.Count(got, "Listed sessions to check for exited agents again") != 1 ||
		strings.Count(got, "Failed to check session "+id+" for an exited agent: executor unreachable") != 1 {
		t.Fatalf("exit watcher log: %q", got)
	}
}
