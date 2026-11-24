package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/google/uuid"
)

// An operation that names content already kept under the same digest
// writes nothing, but still counts as naming it: a removal of the only
// journal that named it keeps it until the operation ends.
func TestRemoveKeepsExistingContentAnOperationNames(t *testing.T) {
	c, dir, _, _, s := setup(t)
	other := uuid.NewString()
	unlock, err := c.lockSession(other)
	if err != nil {
		t.Fatal(err)
	}
	digest := s.Stopped.CheckpointDigest
	if _, err = c.commitCheckpoint(other, s.Input, harness.NativeCodex, testThread, digest); err != nil {
		unlock()
		t.Fatal(err)
	}
	err = c.Remove([]string{s.Source.ExecutionID}, func() ([]string, error) { return nil, nil })
	path := filepath.Join(dir, checkpointPath(digest))
	_, statErr := os.Stat(path)
	unlock()
	if err != nil || statErr != nil {
		t.Fatalf("existing content an operation names was deleted: %v %v", err, statErr)
	}
	if err = c.Remove(nil, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("content no journal names was kept after its operation ended: %v", err)
	}
}

// Removing a settled execution neither waits for another execution's
// operation nor deletes content that operation has written and not yet named
// in its journal; once that operation ends, unnamed content goes.
func TestRemoveRunsBesideOtherOperationsAndKeepsTheirNewContent(t *testing.T) {
	c, dir, _, _, s := setup(t)
	other := uuid.NewString()
	unlock, err := c.lockSession(other)
	if err != nil {
		t.Fatal(err)
	}
	// The operation holds its execution through a long wait, as a graceful
	// stop does, and has retained content its journal does not name yet.
	fresh := []byte("workspace written before its journal")
	digest, err := c.retainWorkspace(other, fresh)
	if err != nil {
		unlock()
		t.Fatal(err)
	}
	began := time.Now()
	err = c.Remove([]string{s.Source.ExecutionID}, func() ([]string, error) { return nil, nil })
	if err != nil || time.Since(began) > 2*time.Second {
		unlock()
		t.Fatalf("remove beside another operation: %v after %s", err, time.Since(began))
	}
	if _, err = os.Stat(filepath.Join(dir, s.Source.ExecutionID+".json")); !os.IsNotExist(err) {
		unlock()
		t.Fatalf("journal kept: %v", err)
	}
	path := filepath.Join(dir, workspacePath(digest))
	if _, err = os.Stat(path); err != nil {
		unlock()
		t.Fatalf("content an operation in progress wrote was deleted: %v", err)
	}
	unlock()
	if err = c.Remove(nil, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("content no journal names was kept after its operation ended: %v", err)
	}
}
