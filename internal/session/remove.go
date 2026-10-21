package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrNotRemovable matches a removal refused because the execution still has a
// writer, an operation in progress or unsettled usage.
var ErrNotRemovable = errors.New("session: execution is not stopped and settled")

// ErrBusy matches a removal that found other operations in progress.
var ErrBusy = errors.New("session: another operation is in progress; try again")

// Removable reports whether an execution has stopped, settled its usage and
// has nothing in progress, so deleting its journal loses no authority or
// accounting evidence that is still needed.
func Removable(r Record) bool {
	return r.Active == nil && r.PendingOperation == "" && (r.Phase == PhaseIdle || r.Phase == PhaseCancelled)
}

// Remove deletes stopped executions' journals, then every retained workspace,
// checkpoint and profile that no remaining journal and no text that release
// returns names. release runs once every execution is known to be removable,
// before any journal is deleted. Remove takes the coordinator exclusively, so no create
// can retain content between the scan and the deletion; it waits at most a
// few seconds for operations in progress and then reports ErrBusy.
func (c *Coordinator) Remove(ids []string, release func() ([]string, error)) error {
	if err := c.lockExclusive(5 * time.Second); err != nil {
		return err
	}
	defer c.mu.Unlock()
	if err := c.ready(); err != nil {
		return err
	}
	for _, id := range ids {
		r, err := c.load(id)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if !Removable(r) {
			return fmt.Errorf("%w: %s is %s", ErrNotRemovable, id, r.Phase)
		}
	}
	// release lets the caller drop its own record of the session once every
	// execution is known to be removable; it returns the records it keeps.
	references, err := release()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := os.Remove(filepath.Join(c.dir, id+".json")); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if id, ok := strings.CutSuffix(entry.Name(), ".json"); ok && validUUID(id) {
			data, err := readFile(filepath.Join(c.dir, entry.Name()), maxJournalBytes)
			if err != nil {
				return err
			}
			references = append(references, string(data))
		}
	}
	// Content is named by its SHA-256 digest, which every record that uses it
	// contains, so a digest no record contains is unreferenced.
	referenced := func(digest string) bool {
		for _, text := range references {
			if strings.Contains(text, digest) {
				return true
			}
		}
		return false
	}
	for _, dir := range []string{checkpointsDir, workspacesDir, profilesDir} {
		files, err := os.ReadDir(filepath.Join(c.dir, dir))
		if err != nil {
			return err
		}
		for _, file := range files {
			digest, _, _ := strings.Cut(file.Name(), ".")
			if validDigest(digest) && !referenced(digest) {
				if err = os.Remove(filepath.Join(c.dir, dir, file.Name())); err != nil && !os.IsNotExist(err) {
					return err
				}
			}
		}
	}
	return nil
}

// lockExclusive takes the coordinator for writing without blocking new
// operations while it waits.
func (c *Coordinator) lockExclusive(wait time.Duration) error {
	deadline := time.Now().Add(wait)
	for !c.mu.TryLock() {
		if time.Now().After(deadline) {
			return ErrBusy
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}
