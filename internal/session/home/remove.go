package home

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/andrewmccall/sproozi/internal/session"
)

// Remove deletes a stopped or cancelled session: its catalogue entry, its
// executions' journals and the retained workspaces, checkpoints and profiles
// nothing else uses. It refuses a session that still has a writer, an
// operation in progress, unsettled usage or an unfinished pull (nextSteps).
// Accounting and grant records, and the placement's evidence of each
// finished visit, are kept. Executions of the session that its entry never
// recorded are released first (releaseOrphans), so none outlives the session.
func (s *Service) Remove(ctx context.Context, id string) (Status, error) {
	unlock, err := s.lockSession(id)
	if err != nil {
		return Status{}, err
	}
	defer unlock()
	status, err := s.Status(id)
	if err != nil {
		return Status{}, err
	}
	status.Session = nil
	catalogued := false
	var executions []string
	var c *localSession
	r, err := s.readCatalog(id)
	switch {
	case err == nil:
		st, _, stateErr := s.sessionState(r)
		if stateErr != nil {
			return Status{}, stateErr
		}
		if err = refusal(id, st, ActionRemove); err != nil {
			return Status{}, err
		}
		if err = s.releaseOrphans(ctx, r); err != nil {
			return Status{}, err
		}
		catalogued, c = true, &r
		executions = slices.Clone(r.Executions)
		if r.Current != "" && !slices.Contains(executions, r.Current) {
			executions = append(executions, r.Current)
		}
	case os.IsNotExist(err):
		execution, inspectErr := s.coordinator.Inspect(id)
		if inspectErr != nil {
			return Status{}, inspectErr
		}
		if execution.Session != "" {
			unlockOwner, recorded, ownerErr := s.lockUnrecorded(execution)
			if ownerErr != nil {
				return Status{}, ownerErr
			}
			defer unlockOwner()
			if recorded {
				return Status{}, rejectedError(fmt.Sprintf("home: %s belongs to session %s; remove that session", id,
					execution.Session))
			}
			if !session.Removable(execution) {
				return Status{}, unrecordedProblem(execution.Session, id)
			}
		}
		executions = []string{id}
	default:
		return Status{}, err
	}
	for _, execution := range executions {
		record, inspectErr := s.coordinator.Inspect(execution)
		if os.IsNotExist(inspectErr) {
			continue
		}
		if inspectErr != nil {
			return Status{}, inspectErr
		}
		if !session.Removable(record) {
			return Status{}, problem(id, derive(c, &record, s.coordinator.PastDeadline))
		}
	}
	err = s.coordinator.Remove(executions, func() ([]string, error) {
		if catalogued {
			if err := s.removeCatalog(id); err != nil {
				return nil, err
			}
		}
		return s.catalogTexts()
	})
	if errors.Is(err, session.ErrNotRemovable) {
		return Status{}, rejectedError(fmt.Sprintf("home: session %s changed while being removed; "+
			"run \"sproozi status %s\"", id, id))
	}
	if errors.Is(err, session.ErrBusy) {
		return Status{}, rejectedError("home: another operation is in progress; run the command again shortly")
	}
	if err != nil {
		return Status{}, err
	}
	return status, s.collectCatalogArtifacts()
}

// lockUnrecorded holds the lock of execution's session, so the session's own
// commands cannot release it meanwhile, and reports whether the session
// records the execution. A session that is gone records nothing.
func (s *Service) lockUnrecorded(execution session.Record) (func(), bool, error) {
	unlock, err := s.lockSession(execution.Session)
	if err != nil {
		return nil, false, err
	}
	owner, err := s.readCatalog(execution.Session)
	switch {
	case os.IsNotExist(err):
		return unlock, false, nil
	case err != nil:
		unlock()
		return nil, false, err
	}
	return unlock, owner.Current == execution.ID || slices.Contains(owner.Executions, execution.ID), nil
}

// removeCatalog deletes a session's catalogue entry.
func (s *Service) removeCatalog(id string) error {
	dir, err := s.catalogDir()
	if err != nil {
		return err
	}
	s.storageMu.Lock()
	defer s.storageMu.Unlock()
	if err = os.Remove(filepath.Join(dir, id+".json")); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// catalogTexts returns every remaining catalogue entry's text, which names the
// checkpoints those sessions still use.
func (s *Service) catalogTexts() ([]string, error) {
	dir, err := s.catalogDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var texts []string
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			data, err := readPrivate(filepath.Join(dir, entry.Name()), 1<<20)
			if err != nil {
				return nil, err
			}
			texts = append(texts, string(data))
		}
	}
	return texts, nil
}

// collectCatalogArtifacts deletes imported checkpoints no catalogue entry names.
func (s *Service) collectCatalogArtifacts() error {
	texts, err := s.catalogTexts()
	if err != nil {
		return err
	}
	dir, err := s.catalogDir()
	if err != nil {
		return err
	}
	s.storageMu.Lock()
	defer s.storageMu.Unlock()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		digest, ok := strings.CutSuffix(entry.Name(), ".checkpoint")
		if !ok || !validDigest(digest) || slices.ContainsFunc(texts, func(t string) bool { return strings.Contains(t, digest) }) {
			continue
		}
		if err = os.Remove(filepath.Join(dir, entry.Name())); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
