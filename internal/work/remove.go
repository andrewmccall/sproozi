package work

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"

	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
)

// removedSuffix names the mark a removed session leaves in the catalogue.
// The mark keeps the session's ID from being used again: a repeated
// submission of the removed session's request is a conflict, never a new
// start, and a repeated removal answers that it is removed. While a removal
// is unfinished the mark lists the session's executions, so it can be
// finished; afterwards it keeps only the ID. It is not a record, so it
// counts towards neither maxSessions nor RetainedBytes.
const removedSuffix = ".removed"

// actionRemove is the owner action that removes a session.
const actionRemove = "remove"

// Removal answers a removal: the executor keeps nothing of the session.
type Removal struct {
	ID string `json:"id"`
}

// removal asks to remove a session. PullFinished states that the caller's
// machine holds the session's pull as finished, which a pulled session
// needs: once removed, the executor can no longer answer a repeated claim.
type removal struct {
	ID           string `json:"id"`
	PullFinished bool   `json:"pullFinished,omitempty"`
}

// removedMark is a removed session's mark. Executions, set until the removal
// is finished, are the session's executions whose journals it deletes.
type removedMark struct {
	Version    int      `json:"version"`
	ID         string   `json:"id"`
	Executions []string `json:"executions,omitempty"`
}

// removable reports a public phase whose session the executor may delete:
// stopped with its usage settled, cancelled, or pulled, whose ownership
// moved to the machine that claimed it. A session being pulled is not: its
// claim is still to come.
func removable(p Phase) bool {
	return p == PhaseRetained || p == PhaseFailed || p == phaseCancelled || p == PhasePulled
}

// Remove deletes a stopped, cancelled or pulled session and everything the
// executor keeps for it: its record, its executions' journals and the
// retained workspaces, checkpoints and profiles nothing else names, through
// the coordinator's Remove, which refuses an execution that is not settled.
// It refuses a session that is starting, running, stopping, has unfinished
// work or is being pulled; a pulled one unless pullFinished; and one that an
// unstarted session starts from. Profiles the placement keeps by digest
// stay.
//
// The mark is written first and durably, then the journals go, then the
// record, and the mark shrinks last; Open finishes a removal a crash
// interrupted before it starts any session. Removing again answers removed.
func (o *Owner) Remove(ctx context.Context, id string, pullFinished bool) (Removal, error) {
	if !CanonicalID(id) {
		return Removal{}, &profile.Rejection{Code: profile.CodeInvalid}
	}
	if ctx.Err() != nil {
		return Removal{}, ctx.Err()
	}
	unlock := o.jobs.Lock(id)
	defer unlock()
	// Serialized with admission, so a submission cannot start from this
	// session between the check of its dependants and the removal.
	o.retention.Lock()
	defer o.retention.Unlock()
	mark, marked, err := o.readRemoved(id)
	if err != nil {
		return Removal{}, err
	}
	if marked {
		// Removed, or interrupted after its checks: finish it.
		return Removal{ID: id}, o.finishRemoval(id, mark.Executions)
	}
	e, err := o.entry(id)
	if err != nil {
		return Removal{}, err
	}
	if err = o.access(e.association()); err != nil {
		return Removal{}, err
	}
	var executions []string
	if e.legacy != nil {
		phase := e.legacy.Phase
		if phase == legacyExporting || !phase.stoppedOrCancelled() || phase == legacyPulled && !pullFinished {
			return Removal{}, &profile.Rejection{Code: profile.CodeConflict}
		}
	} else {
		r := *e.current
		rec, err := o.execution(r)
		if err != nil {
			return Removal{}, err
		}
		phase := derivePhase(r, rec)
		if !removable(phase) || phase == PhasePulled && !pullFinished {
			return Removal{}, &profile.Rejection{Code: profile.CodeConflict}
		}
		executions = r.Executions
	}
	if err = o.checkNoDependants(id); err != nil {
		return Removal{}, err
	}
	if err = o.markRemoved(removedMark{ID: id, Executions: executions}); err != nil {
		return Removal{}, err
	}
	if err = o.finishRemoval(id, executions); err != nil {
		return Removal{}, err
	}
	o.logf("Removed work session %s", id)
	return Removal{ID: id}, nil
}

// checkNoDependants refuses to remove id while a session the coordinator has
// not received yet starts from it: its start reads id's checkpoint.
func (o *Owner) checkNoDependants(id string) error {
	ids, err := o.ids()
	if err != nil {
		return err
	}
	for _, other := range ids {
		r, err := o.read(other)
		if err != nil || r.Intent.WorkspaceFrom == nil || r.Intent.WorkspaceFrom.SessionID != id ||
			r.CancelOperation != "" || r.Transfer != "" {
			continue
		}
		if rec, err := o.execution(r); err != nil || rec == nil {
			return &profile.Rejection{Code: profile.CodeConflict}
		}
	}
	return nil
}

// finishRemoval deletes, in order, the executions' journals and the content
// only they named, then the session's own files, the record last, and then
// shrinks the mark to the ID. Each step is idempotent, so a repeated or
// reopened removal finishes from wherever it stopped.
func (o *Owner) finishRemoval(id string, executions []string) error {
	if len(executions) > 0 {
		// The only coordinator content a catalogue record names is the
		// checkpoint of the session its workspaceFrom starts from, which that
		// session's journals name too; checkNoDependants keeps that session
		// until the dependant has started. So no record adds references.
		err := o.config.Coordinator.Remove(executions, func() ([]string, error) { return nil, nil })
		switch {
		case errors.Is(err, session.ErrNotRemovable):
			return &profile.Rejection{Code: profile.CodeConflict}
		case err != nil:
			o.logf("Could not remove work session %s: %v", id, err)
			return err
		}
	}
	// A version 1 record kept its checkpoint and input beside it.
	for _, suffix := range []string{uploadSuffix, ".checkpoint", ".input", ".json"} {
		if err := o.removeFile(id, suffix); err != nil {
			return err
		}
	}
	if err := privatefs.SyncDir(o.config.Directory); err != nil {
		return err
	}
	if len(executions) == 0 {
		return nil
	}
	return o.markRemoved(removedMark{ID: id})
}

// finishRemovals finishes every removal a crash interrupted: a mark that
// still lists executions, or whose record is still there. Open runs it
// before it starts any session.
func (o *Owner) finishRemovals() error {
	entries, err := os.ReadDir(o.config.Directory)
	if err != nil {
		return err
	}
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), removedSuffix)
		if !ok || !CanonicalID(id) {
			continue
		}
		mark, _, err := o.readRemoved(id)
		if err != nil {
			return err
		}
		if _, err = os.Lstat(o.path(id, ".json")); len(mark.Executions) == 0 && os.IsNotExist(err) {
			continue
		}
		if err = o.finishRemoval(id, mark.Executions); err != nil {
			return err
		}
		o.logf("Finished removing work session %s", id)
	}
	return nil
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
	if m.Version != 1 || m.ID != id || len(m.Executions) > maxExecutions {
		return m, false, errors.New("work: removed session mark is inconsistent")
	}
	for _, execution := range m.Executions {
		if !CanonicalID(execution) {
			return m, false, errors.New("work: removed session mark is inconsistent")
		}
	}
	return m, true, nil
}

// recorded reports a session whose record is kept.
func (o *Owner) recorded(id string) bool {
	_, err := os.Lstat(o.path(id, ".json"))
	return err == nil
}

// wasRemoved reports a session that was removed, so its ID is not used
// again.
func (o *Owner) wasRemoved(id string) bool {
	_, err := os.Lstat(o.path(id, removedSuffix))
	return !os.IsNotExist(err)
}
