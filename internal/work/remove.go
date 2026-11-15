package work

import (
	"context"
	"encoding/json"
	"errors"
	"os"

	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
)

// removedSuffix names the mark a removed session leaves in the catalogue.
// The mark keeps the session's ID from being used again: a repeated
// submission of the removed session's request is a conflict, never a new
// start, and a repeated removal answers that it is removed. It is not a
// record, so it counts towards neither maxSessions nor RetainedBytes; it is
// a few hundred bytes.
const removedSuffix = ".removed"

// actionRemove is the owner action that removes a session.
const actionRemove = "remove"

// Removal answers a removal: the executor keeps nothing of the session.
type Removal struct {
	ID string `json:"id"`
}

// removedMark is a removed session's mark. Executions are its executions,
// so a removal interrupted after the record went still deletes their
// journals when it is repeated.
type removedMark struct {
	Version    int      `json:"version"`
	ID         string   `json:"id"`
	Executions []string `json:"executions,omitempty"`
}

// removable reports a public phase whose session the executor may delete:
// stopped with its usage settled, or cancelled, or pulled, whose ownership
// moved to the machine that claimed it. A session being pulled is not: its
// claim is still to come.
func removable(p Phase) bool {
	return p == PhaseRetained || p == PhaseFailed || p == phaseCancelled || p == PhasePulled
}

// Remove deletes a stopped, cancelled or pulled session and everything the
// executor keeps for it: its record, its executions' journals and the
// retained workspaces, checkpoints and profiles nothing else uses, through
// the coordinator's Remove, which refuses an execution that is not settled.
// It refuses a session that is starting, running, stopping, has unfinished
// work or is being pulled. Accounting kept outside the executor, and
// profiles the placement keeps by digest, stay.
//
// It is replay-safe: the session leaves a mark, and removing it again
// finishes an interrupted removal and answers that it is removed.
func (o *Owner) Remove(ctx context.Context, id string) (Removal, error) {
	if !CanonicalID(id) {
		return Removal{}, &profile.Rejection{Code: profile.CodeInvalid}
	}
	if ctx.Err() != nil {
		return Removal{}, ctx.Err()
	}
	unlock := o.jobs.Lock(id)
	defer unlock()
	mark, marked, err := o.readRemoved(id)
	if err != nil {
		return Removal{}, err
	}
	if _, err = os.Lstat(o.path(id, ".json")); os.IsNotExist(err) {
		if !marked {
			return Removal{}, &profile.Rejection{Code: profile.CodeInvalid}
		}
		return Removal{ID: id}, o.removeExecutions(id, mark.Executions, func() error { return nil })
	} else if err != nil {
		return Removal{}, err
	}
	e, err := o.entry(id)
	if err != nil {
		return Removal{}, err
	}
	if err = o.access(e.association()); err != nil {
		return Removal{}, err
	}
	if e.legacy != nil {
		return o.removeLegacy(*e.legacy)
	}
	r := *e.current
	rec, err := o.execution(r)
	if err != nil {
		return Removal{}, err
	}
	if !removable(derivePhase(r, rec)) {
		return Removal{}, &profile.Rejection{Code: profile.CodeConflict}
	}
	err = o.removeExecutions(id, r.Executions, func() error {
		if err := o.markRemoved(removedMark{ID: id, Executions: r.Executions}); err != nil {
			return err
		}
		o.removeUpload(id)
		return o.removeFile(id, ".json")
	})
	if err != nil {
		return Removal{}, err
	}
	o.logf("Removed work session %s", id)
	return Removal{ID: id}, nil
}

// removeExecutions deletes executions through the coordinator, which first
// checks that each is settled; release deletes the session's own files once
// they are, before any journal goes.
func (o *Owner) removeExecutions(id string, executions []string, release func() error) error {
	err := o.config.Coordinator.Remove(executions, func() ([]string, error) {
		// Catalogue records name no content the coordinator keeps: a
		// session's content is named by its executions' journals.
		return nil, release()
	})
	switch {
	case errors.Is(err, session.ErrNotRemovable):
		return &profile.Rejection{Code: profile.CodeConflict}
	case err != nil:
		o.logf("Could not remove work session %s: %v", id, err)
	}
	return err
}

// removeLegacy deletes a stopped or cancelled version 1 record and the
// checkpoint and input it kept; the coordinator holds nothing of it. One
// being pulled is refused. The caller holds the session's lock.
func (o *Owner) removeLegacy(r legacyRecord) (Removal, error) {
	id := r.Launch.ExecutionID
	if r.Phase == legacyExporting || !r.Phase.stoppedOrCancelled() {
		return Removal{}, &profile.Rejection{Code: profile.CodeConflict}
	}
	if err := o.markRemoved(removedMark{ID: id}); err != nil {
		return Removal{}, err
	}
	// The record goes last, so a repeated removal finds what is left.
	for _, suffix := range []string{".checkpoint", ".input", ".json"} {
		if err := o.removeFile(id, suffix); err != nil {
			return Removal{}, err
		}
	}
	o.logf("Removed work session %s", id)
	return Removal{ID: id}, nil
}

func (o *Owner) removeFile(id, suffix string) error {
	if err := os.Remove(o.path(id, suffix)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (o *Owner) markRemoved(m removedMark) error {
	m.Version = 1
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return privatefs.Replace(o.config.Directory, m.ID+removedSuffix, b)
}

// readRemoved reads id's mark, and whether there is one.
func (o *Owner) readRemoved(id string) (removedMark, bool, error) {
	var m removedMark
	b, err := privatefs.ReadFile(o.path(id, removedSuffix), maxRecordBytes)
	if os.IsNotExist(err) {
		return m, false, nil
	}
	if err != nil {
		return m, false, err
	}
	if err = decode(b, &m); err != nil {
		return m, false, err
	}
	if m.Version != 1 || m.ID != id {
		return m, false, errors.New("work: removed session mark is inconsistent")
	}
	for _, execution := range m.Executions {
		if !CanonicalID(execution) {
			return m, false, errors.New("work: removed session mark is inconsistent")
		}
	}
	return m, true, nil
}

// wasRemoved reports a session that was removed, so its ID is not used
// again.
func (o *Owner) wasRemoved(id string) bool {
	_, err := os.Lstat(o.path(id, removedSuffix))
	return err == nil
}
