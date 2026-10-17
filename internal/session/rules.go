package session

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

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
	if l.Harness.Validate() != nil || !validUUID(l.SessionID) || !validUUID(l.OperationID) || !validUUID(l.AllocationID) || !validUUID(l.ThreadID) || !validName(l.Placement) || l.Epoch == 0 || !validDigest(l.InstructionDigest) || l.Deadline.IsZero() || (!validDigest(l.CheckpointDigest) && l.CheckpointDigest != "") {
		return errors.New("session: invalid launch identity")
	}
	if err := workspace.ValidateIgnoredPaths(l.IncludeIgnored); err != nil {
		return err
	}
	return validateAllowance(l.Allowances)
}
func sameLaunch(a, b Launch) bool {
	a.Deadline = a.Deadline.UTC()
	b.Deadline = b.Deadline.UTC()
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
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
	if !sameLaunch(launch, receipt.Launch) || inc != receipt.Incarnation {
		return errors.New("session: receipt allocation/incarnation mismatch")
	}
	_, err := subtract(launch.Allowances, *receipt)
	return err
}
func validateStop(launch Launch, inc Incarnation, s Stopped) error {
	if !sameLaunch(launch, s.Launch) || inc != s.Incarnation || !s.ActualStopped {
		return errors.New("session: source stop remains unknown")
	}
	return validateReceipt(launch, inc, s.Receipt)
}
func launchBelongs(r Record, l Launch) bool {
	return l.Harness == r.Harness && l.SessionID == r.ID && l.ThreadID == r.ThreadID && l.InstructionDigest == r.InstructionDigest && l.Deadline.Equal(r.Deadline) && slices.Equal(l.IncludeIgnored, r.IncludeIgnored)
}
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
	if r.Harness.Validate() != nil || r.Version != journalVersion || !validUUID(r.ID) || !validUUID(r.ThreadID) || !validDigest(r.InstructionDigest) || r.Deadline.IsZero() || r.Epoch == 0 || len(r.Operations) > maxOperations || len(r.Visits) == 0 {
		return errors.New("session: invalid journal identity")
	}
	if err := validateAllowance(r.Ceiling); err != nil {
		return err
	}
	if err := validateAllowance(r.Remaining); err != nil {
		return err
	}
	if r.Checkpoint.Metadata.Harness != r.Harness || r.Checkpoint.Metadata.ThreadID != r.ThreadID || !validDigest(r.Checkpoint.Metadata.SHA256) || r.Checkpoint.Path != "checkpoints/"+r.Checkpoint.Metadata.SHA256+".json" || r.Checkpoint.Metadata.Bytes <= 0 {
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
		if !launchBelongs(r, visit.Launch) || !validIncarnation(visit.Incarnation) || seenAlloc[visit.Launch.AllocationID] || !visit.ActualStopped || !validDigest(visit.CheckpointDigest) || visit.Launch.Epoch <= lastEpoch {
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
	ids := map[string]bool{}
	pending := false
	for _, op := range r.Operations {
		if !validUUID(op.ID) || ids[op.ID] ||
			(!op.Finalize && !validName(op.Destination)) ||
			(op.Finalize && (op.Destination != "" || op.Launch != nil || op.Incarnation != nil)) {
			return errors.New("session: invalid retained operation")
		}
		ids[op.ID] = true
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
		if op.ID == r.PendingOperation {
			pending = !op.Complete
		}
	}
	if r.PendingOperation != "" && !pending {
		return errors.New("session: missing pending operation")
	}
	return nil
}
func validatePhase(r Record) error {
	switch r.Phase {
	case PhaseIdle, PhaseActive, PhaseStopping, PhaseUnknownStop, PhaseUnknownUsage, PhasePreparing, PhaseActivating, PhaseCancelled:
	default:
		return fmt.Errorf("session: invalid phase %q", r.Phase)
	}
	if r.CancelOperation != "" && (!validUUID(r.CancelOperation) || r.Phase != PhaseCancelled) {
		return errors.New("session: invalid cancellation intent")
	}
	return nil
}
func beforeDeadline(l Launch) error {
	if !time.Now().Before(l.Deadline) {
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
