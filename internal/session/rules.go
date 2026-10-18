package session

import (
	"cmp"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/workspace"
	"github.com/google/uuid"
)

func validUUID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u.String() == s && u != uuid.Nil
}
func validName(s string) bool {
	if len(s) == 0 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '-' {
			return false
		}
	}
	return true
}
func validDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && strings.ToLower(s) == s
}
func validIncarnation(i Incarnation) bool {
	return validName(i.ExecutionID) && validName(i.ResourceUID)
}
func validLimit(l Limit) bool { return l.Value >= 0 && (l.Bounded || l.Value == 0) }
func validateAllowance(a map[string]Allowance) error {
	if len(a) == 0 || len(a) > 32 {
		return errors.New("session: capability allowances required")
	}
	for name, limit := range a {
		if !validName(name) || !validLimit(limit.Units) || !validLimit(limit.CostMicros) {
			return errors.New("session: invalid capability allowance")
		}
	}
	return nil
}
func validateLaunch(l Launch) error {
	// Only a first visit from a Git workspace lacks a thread, and it then has
	// no checkpoint either; every other launch names both.
	fresh := l.ThreadID == "" && l.CheckpointDigest == ""
	if l.Harness.Validate() != nil || !validUUID(l.ExecutionID) || !validUUID(l.OperationID) || !validUUID(l.AllocationID) || !fresh && (!validUUID(l.ThreadID) || !validDigest(l.CheckpointDigest)) || !validName(l.Placement) || l.Epoch == 0 || !validDigest(l.InstructionDigest) || l.Deadline.IsZero() {
		return errors.New("session: invalid launch identity")
	}
	if err := workspace.ValidateIgnoredPaths(l.IncludeIgnored); err != nil {
		return err
	}
	return validateAllowance(l.Allowances)
}

// SameLaunch is the one equality for immutable launch intent. JSON encoding
// normalises map order and empty slices; deadlines compare in UTC.
func SameLaunch(a, b Launch) bool {
	a.Deadline = a.Deadline.UTC()
	b.Deadline = b.Deadline.UTC()
	x, errA := json.Marshal(a)
	y, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(x) == string(y)
}
func cloneAllowances(a map[string]Allowance) map[string]Allowance {
	copy := make(map[string]Allowance, len(a))
	maps.Copy(copy, a)
	return copy
}
func sameAllowances(a, b map[string]Allowance) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if value, ok := b[k]; !ok || value != v {
			return false
		}
	}
	return true
}

// CheckUsage is the one sealed-usage rule shared by placements, transports and
// the coordinator: usage names exactly the allocated capabilities, is never
// negative and stays within every bounded ceiling.
func CheckUsage(allowances map[string]Allowance, usage map[string]budget.TrustedUsage) error {
	if len(usage) != len(allowances) {
		return errors.New("session: incomplete accounting receipt")
	}
	for name, ceiling := range allowances {
		used, ok := usage[name]
		if !ok || used.Units < 0 || used.CostMicros < 0 {
			return errors.New("session: invalid trusted usage")
		}
		if ceiling.Units.Bounded && used.Units > ceiling.Units.Value {
			return errors.New("session: receipt exceeds units allocation")
		}
		if ceiling.CostMicros.Bounded && used.CostMicros > ceiling.CostMicros.Value {
			return errors.New("session: receipt exceeds cost allocation")
		}
	}
	return nil
}
func subtract(remaining map[string]Allowance, receipt Receipt) (map[string]Allowance, error) {
	if err := CheckUsage(remaining, receipt.Usage); err != nil {
		return nil, err
	}
	next := cloneAllowances(remaining)
	for name, current := range remaining {
		usage := receipt.Usage[name]
		if current.Units.Bounded {
			current.Units.Value -= usage.Units
		}
		if current.CostMicros.Bounded {
			current.CostMicros.Value -= usage.CostMicros
		}
		next[name] = current
	}
	return next, nil
}
func validateReceipt(launch Launch, inc Incarnation, receipt *Receipt) error {
	if receipt == nil {
		return errors.New("session: accounting remains unknown")
	}
	if !SameLaunch(launch, receipt.Launch) || inc != receipt.Incarnation {
		return errors.New("session: receipt allocation/incarnation mismatch")
	}
	_, err := subtract(launch.Allowances, *receipt)
	return err
}
func validateStop(launch Launch, inc Incarnation, s Stopped) error {
	if !fencedBy(launch, inc, s) {
		return errors.New("session: source stop remains unknown")
	}
	return validateReceipt(launch, inc, s.Receipt)
}

// fencedBy reports that s proves the exact launch and incarnation stopped and
// names a thread consistent with the launch.
func fencedBy(launch Launch, inc Incarnation, s Stopped) bool {
	thread := s.ThreadID == "" || s.ThreadID == launch.ThreadID || launch.ThreadID == "" && validUUID(s.ThreadID)
	return s.ActualStopped && SameLaunch(launch, s.Launch) && inc == s.Incarnation && thread
}

// stoppedThread is the native thread a stopped visit ran.
func stoppedThread(s Stopped) string { return cmp.Or(s.ThreadID, s.Launch.ThreadID) }

// launchBelongs checks a launch against the execution's fixed identity. Only
// the first visit of an execution started from a workspace lacks the thread.
func launchBelongs(r Record, l Launch) bool {
	thread := l.ThreadID == r.ThreadID || l.ThreadID == "" && l.Epoch == 1 && r.Workspace != ""
	return thread && l.Harness == r.Harness && l.ExecutionID == r.ID && l.InstructionDigest == r.InstructionDigest && l.Deadline.Equal(r.Deadline) && slices.Equal(l.IncludeIgnored, r.IncludeIgnored)
}

func (r Record) hasCheckpoint() bool { return r.Checkpoint.Path != "" }
func validateRecord(r Record) error {
	if err := validateJournalIdentity(r); err != nil {
		return err
	}
	lastEpoch, seenAlloc, err := validateHistory(r)
	if err != nil {
		return err
	}
	if err = validateActive(r, lastEpoch, seenAlloc); err != nil {
		return err
	}
	if err = validateOperations(r); err != nil {
		return err
	}
	return validatePhase(r)
}
func validateJournalIdentity(r Record) error {
	if err := workspace.ValidateIgnoredPaths(r.IncludeIgnored); err != nil {
		return err
	}
	if r.Harness.Validate() != nil || r.Version != journalVersion || !validUUID(r.ID) || r.Session != "" && !validUUID(r.Session) || !validDigest(r.InstructionDigest) || r.Deadline.IsZero() || len(r.Operations) == 0 || len(r.Operations) > maxOperations {
		return errors.New("session: invalid journal identity")
	}
	if r.Workspace != "" && !validDigest(r.Workspace) {
		return errors.New("session: invalid retained workspace")
	}
	if err := validateAllowance(r.Ceiling); err != nil {
		return err
	}
	if err := validateAllowance(r.Remaining); err != nil {
		return err
	}
	// The thread and its checkpoint are committed together: from the first
	// visit's input when resuming, otherwise by the first stop.
	if !r.hasCheckpoint() {
		if r.ThreadID != "" || r.Workspace == "" || r.Checkpoint != (Checkpoint{}) {
			return errors.New("session: execution has neither checkpoint nor workspace input")
		}
		return nil
	}
	if !validUUID(r.ThreadID) || r.Checkpoint.Metadata.Harness != r.Harness || r.Checkpoint.Metadata.ThreadID != r.ThreadID || !validDigest(r.Checkpoint.Metadata.SHA256) || r.Checkpoint.Path != checkpointPath(r.Checkpoint.Metadata.SHA256) || r.Checkpoint.Metadata.Bytes <= 0 {
		return errors.New("session: invalid committed checkpoint")
	}
	return nil
}
func validateHistory(r Record) (uint64, map[string]bool, error) {
	remaining := cloneAllowances(r.Ceiling)
	var lastEpoch uint64
	seenAlloc := map[string]bool{}
	for _, visit := range r.Visits {
		if err := validateLaunch(visit.Launch); err != nil {
			return 0, nil, err
		}
		// A visit cancelled before any checkpoint existed retains none.
		checkpointed := validDigest(visit.CheckpointDigest) || visit.CheckpointDigest == "" && !r.hasCheckpoint()
		if !launchBelongs(r, visit.Launch) || !validIncarnation(visit.Incarnation) || seenAlloc[visit.Launch.AllocationID] || !visit.ActualStopped || !checkpointed || visit.Launch.Epoch <= lastEpoch {
			return 0, nil, errors.New("session: invalid retained visit")
		}
		if !sameAllowances(remaining, visit.Launch.Allowances) {
			return 0, nil, errors.New("session: allocation ceiling continuity mismatch")
		}
		if err := validateReceipt(visit.Launch, visit.Incarnation, visit.Receipt); err != nil {
			return 0, nil, err
		}
		var err error
		remaining, err = subtract(remaining, *visit.Receipt)
		if err != nil {
			return 0, nil, err
		}
		seenAlloc[visit.Launch.AllocationID] = true
		lastEpoch = visit.Launch.Epoch
	}
	if !sameAllowances(remaining, r.Remaining) {
		return 0, nil, errors.New("session: journal remaining allowance mismatch")
	}
	return lastEpoch, seenAlloc, nil
}
func validateActive(r Record, lastEpoch uint64, seenAlloc map[string]bool) error {
	if r.Active != nil {
		if err := validateLaunch(r.Active.Launch); err != nil {
			return err
		}
		if !launchBelongs(r, r.Active.Launch) || !validIncarnation(r.Active.Incarnation) || seenAlloc[r.Active.Launch.AllocationID] || !sameAllowances(r.Remaining, r.Active.Launch.Allowances) || r.Active.Launch.Epoch != r.Epoch || r.Active.Launch.Epoch <= lastEpoch {
			return errors.New("session: invalid active allocation")
		}
	}
	if r.Active == nil && r.Epoch != lastEpoch {
		return errors.New("session: journal epoch continuity mismatch")
	}
	return nil
}
func validateOperations(r Record) error {
	if first := r.Operations[0]; first.Finalize || first.Launch == nil || first.Launch.Epoch != 1 ||
		(first.Launch.CheckpointDigest == "") != (r.Workspace != "") {
		return errors.New("session: execution lacks its first visit")
	}
	ids := map[string]bool{}
	for i, op := range r.Operations {
		if !validUUID(op.ID) || ids[op.ID] ||
			(!op.Finalize && !validName(op.Destination)) ||
			(op.Finalize && (op.Destination != "" || op.Launch != nil || op.Incarnation != nil)) {
			return errors.New("session: invalid retained operation")
		}
		ids[op.ID] = true
		switch op.Stage {
		case stagePrepare, stageActivate, stageStop, stageComplete, stageCancelled:
		default:
			return errors.New("session: invalid operation stage")
		}
		// Only the last operation can be unfinished, and only while pending.
		if !op.done() && (i != len(r.Operations)-1 || op.ID != r.PendingOperation) {
			return errors.New("session: unfinished operation is not pending")
		}
		if op.Launch != nil {
			if err := validateLaunch(*op.Launch); err != nil {
				return err
			}
			if !launchBelongs(r, *op.Launch) || op.Launch.OperationID != op.ID || op.Launch.Placement != op.Destination {
				return errors.New("session: operation binding mismatch")
			}
		}
		if op.Incarnation != nil && !validIncarnation(*op.Incarnation) {
			return errors.New("session: invalid prepared incarnation")
		}
	}
	if r.PendingOperation != "" {
		if len(r.Operations) == 0 || r.Operations[len(r.Operations)-1].ID != r.PendingOperation || r.Operations[len(r.Operations)-1].done() {
			return errors.New("session: missing pending operation")
		}
	}
	return nil
}

// pendingStage is the next step of the pending operation, or "" when none.
func pendingStage(r Record) Stage {
	if r.PendingOperation == "" {
		return ""
	}
	return r.Operations[len(r.Operations)-1].Stage
}

// phaseFacts is the structure a phase summarises.
type phaseFacts struct {
	pending Stage // next step of the pending operation, or "" when none
	writer  bool  // an active visit exists, running or fenced
	running bool  // the active writer may still run
	fenced  bool  // the active writer is proven stopped
	cancel  bool  // cancellation was requested
}

// phaseRules says, for each phase, which structures it may describe.
var phaseRules = map[Phase]func(phaseFacts) bool{
	PhaseIdle:       func(f phaseFacts) bool { return !f.writer && f.pending == "" && !f.cancel },
	PhasePreparing:  func(f phaseFacts) bool { return !f.writer && f.pending == stagePrepare && !f.cancel },
	PhaseActivating: func(f phaseFacts) bool { return !f.writer && f.pending == stageActivate && !f.cancel },
	PhaseActive:     func(f phaseFacts) bool { return f.running && f.pending == "" && !f.cancel },
	PhaseStopping:   func(f phaseFacts) bool { return f.running && f.pending == stageStop && !f.cancel },
	PhaseUnknownStop: func(f phaseFacts) bool {
		return f.running && (f.pending == stageStop || f.cancel) || !f.writer && f.pending != "" && f.cancel
	},
	PhaseUnknownUsage: func(f phaseFacts) bool { return f.fenced && (f.pending == stageStop || f.cancel) },
	PhaseCancelling:   func(f phaseFacts) bool { return f.cancel },
	PhaseCancelled:    func(f phaseFacts) bool { return f.cancel && !f.writer && f.pending == "" },
}

// validatePhase checks that the phase describes the record's actual structure:
// whether a writer may run (Active), whether it is fenced (ActualStopped),
// which step is pending and whether cancellation was requested.
func validatePhase(r Record) error {
	if r.CancelOperation != "" && !validUUID(r.CancelOperation) {
		return errors.New("session: invalid cancellation intent")
	}
	rule, ok := phaseRules[r.Phase]
	if !ok {
		return fmt.Errorf("session: invalid phase %q", r.Phase)
	}
	facts := phaseFacts{pending: pendingStage(r), writer: r.Active != nil, cancel: r.CancelOperation != ""}
	facts.running = facts.writer && !r.Active.ActualStopped
	facts.fenced = facts.writer && r.Active.ActualStopped
	if !rule(facts) {
		return fmt.Errorf("session: phase %q contradicts the retained execution", r.Phase)
	}
	return nil
}
func (c *Coordinator) beforeDeadline(l Launch) error {
	if !c.now().Before(l.Deadline) {
		return errors.New("session: original deadline expired")
	}
	return nil
}
func spendable(a map[string]Allowance) error {
	for _, v := range a {
		if v.Units.Bounded && v.Units.Value == 0 || v.CostMicros.Bounded && v.CostMicros.Value == 0 {
			return errors.New("session: allowance exhausted")
		}
	}
	return nil
}
