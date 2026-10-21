package home

import (
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
// operation in progress or unsettled usage. Accounting and grant records, and
// the placement's evidence of each finished visit, are kept.
func (s *Service) Remove(id string) (Status, error) {
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
	r, err := s.readCatalog(id)
	switch {
	case err == nil:
		if !r.Claimed {
			return Status{}, rejectedError(fmt.Sprintf("home: session %s is still being pulled; "+
				"finish with \"sproozi pull %s\" before removing it", id, id))
		}
		catalogued = true
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
			return Status{}, rejectedError(fmt.Sprintf("home: %s belongs to session %s; remove that session", id,
				execution.Session))
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
			return Status{}, notRemovable(id, record)
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

// notRemovable says why a session cannot be removed yet and what would allow it.
func notRemovable(id string, r session.Record) error {
	switch {
	case r.Phase == session.PhaseUnknownStop || r.Phase == session.PhaseUnknownUsage || r.Phase == session.PhaseCancelling:
		return rejectedError(fmt.Sprintf("home: session %s has unfinished work (%s); run \"sproozi recover %s\", "+
			"then remove it", id, r.Phase, id))
	case r.Active != nil:
		return rejectedError(fmt.Sprintf("home: session %s is running; stop it with \"sproozi stop %s\" "+
			"or \"sproozi cancel %s\" first", id, id, id))
	}
	return rejectedError(fmt.Sprintf("home: session %s is %s; wait for it to finish, then remove it", id, r.Phase))
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
