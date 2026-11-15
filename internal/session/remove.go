package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/privatefs"
)

// ErrNotRemovable matches a removal refused because the execution still has a
// writer, an operation in progress or unsettled usage.
var ErrNotRemovable = errors.New("session: execution is not stopped and settled")

// ErrBusy matches a removal that found an operation in progress on one of the
// executions it removes.
var ErrBusy = errors.New("session: another operation is in progress; try again")

// Removable reports whether an execution has stopped, settled its usage and
// has nothing in progress, so deleting its journal loses no authority or
// accounting evidence that is still needed. A loaded record's phase always
// describes its structure, so its phase alone decides.
func Removable(r Record) bool { return r.Phase.Settled() }

// RemovalRefusal says why the session id, whose execution is in phase,
// cannot be removed yet and which command allows it. command spells a
// command, such as "recover", for this session wherever it is: home's
// service and a remote executor's sessions share these words.
func RemovalRefusal(id string, phase Phase, command func(name string) string) string {
	switch {
	case phase.Unresolved():
		return fmt.Sprintf("session %s has unfinished work (%s); run \"%s\", then remove it", id, phase,
			command("recover"))
	case phase == PhaseActive || phase == PhaseStopping:
		return fmt.Sprintf("session %s is running; stop it with \"%s\" or \"%s\" first", id, command("stop"),
			command("cancel"))
	}
	return fmt.Sprintf("session %s is %s; wait for it to finish, then remove it", id, phase)
}

// Remove deletes stopped executions' journals, then every retained workspace,
// checkpoint and profile that no remaining journal, no text that release
// returns and no operation in progress names. release runs once every
// execution is known to be removable, before any journal is deleted.
//
// Remove takes only the removed executions' own locks, in ID order, so it
// runs beside operations on other executions however long they wait for a
// native turn. It waits at most a few seconds for an operation on one of the
// removed executions, then reports ErrBusy. Content clean-up runs under the
// content lock, which operations take only while they write content and
// while they forget what they wrote (see Coordinator.fresh), never across a
// placement call.
func (c *Coordinator) Remove(ids []string, release func() ([]string, error)) error {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if err := c.ready(); err != nil {
		return err
	}
	unlock, err := c.lockJournals(ids, 5*time.Second)
	if err != nil {
		return err
	}
	references, err := c.removeJournals(ids, release)
	unlock()
	if err != nil {
		return err
	}
	return c.collect(references)
}

// removeJournals deletes the journals of executions whose locks the caller
// holds, once each is removable and release has run.
func (c *Coordinator) removeJournals(ids []string, release func() ([]string, error)) ([]string, error) {
	for _, id := range ids {
		r, err := c.load(id)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !Removable(r) {
			return nil, fmt.Errorf("%w: %s is %s", ErrNotRemovable, id, r.Phase)
		}
	}
	// release lets the caller drop its own record of the session once every
	// execution is known to be removable; it returns the records it keeps.
	references, err := release()
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if err := os.Remove(filepath.Join(c.dir, id+".json")); err != nil && !os.IsNotExist(err) {
			return nil, err
		}
	}
	return references, privatefs.SyncDir(c.dir)
}

// collect deletes retained content that no journal, no reference and no
// operation in progress names. It holds the content lock, so no operation
// writes content or forgets what it wrote while the journals are read.
func (c *Coordinator) collect(references []string) error {
	c.content.Lock()
	defer c.content.Unlock()
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if id, ok := strings.CutSuffix(entry.Name(), ".json"); ok && validUUID(id) {
			data, err := readFile(filepath.Join(c.dir, entry.Name()), maxJournalBytes)
			if os.IsNotExist(err) {
				// Removed beside this clean-up.
				continue
			}
			if err != nil {
				return err
			}
			references = append(references, string(data))
		}
	}
	c.freshMu.Lock()
	for _, paths := range c.fresh {
		references = append(references, paths...)
	}
	c.freshMu.Unlock()
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

// lockJournals takes the executions' locks in ID order, waiting at most wait
// in all, and never blocks operations on other executions.
func (c *Coordinator) lockJournals(ids []string, wait time.Duration) (func(), error) {
	sorted := slices.Clone(ids)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)
	var unlocks []func()
	unlock := func() {
		for i := len(unlocks) - 1; i >= 0; i-- {
			unlocks[i]()
		}
	}
	deadline := time.Now().Add(wait)
	for _, id := range sorted {
		if !validUUID(id) {
			unlock()
			return nil, errors.New("session: invalid session ID")
		}
		for {
			if u, ok := c.journals.TryLock(id); ok {
				unlocks = append(unlocks, u)
				break
			}
			if time.Now().After(deadline) {
				unlock()
				return nil, ErrBusy
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	return unlock, nil
}
