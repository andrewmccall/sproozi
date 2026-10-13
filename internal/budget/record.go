package budget

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// ledgerState retains an optional immutable seal alongside the original four
// counters. Existing unsealed records keep their stored format.
type ledgerState struct {
	State
	Sealed *TrustedUsage `json:"sealed,omitempty"`
	// Omission means legacy reservation history is unknown, not zero. Keep
	// ordinary accounting available, but never seal that uncertainty away.
	Pending   *int64 `json:"pendingReservations,omitempty"`
	Unbounded *int64 `json:"pendingUnboundedReservations,omitempty"`
}

// Record is the opaque durable accounting state passed between ledger rules and
// atomic storage. Storage adapters serialize it; they do not change its counters.
type Record struct{ state ledgerState }

func NewRecord() *Record {
	pending, unbounded := int64(0), int64(0)
	return &Record{state: ledgerState{Pending: &pending, Unbounded: &unbounded}}
}

func (r *Record) Encode() (string, error) {
	if err := validateLedgerState(r.state); err != nil {
		return "", err
	}
	data, err := json.Marshal(r.state)
	return string(data), err
}

func DecodeRecord(data string) (*Record, error) {
	state, err := decodeLedgerState(data)
	if err != nil {
		return nil, err
	}
	return &Record{state: state}, nil
}

func cloneRecord(r *Record) *Record {
	copy := *r
	if r.state.Pending != nil {
		n := *r.state.Pending
		copy.state.Pending = &n
	}
	if r.state.Unbounded != nil {
		n := *r.state.Unbounded
		copy.state.Unbounded = &n
	}
	if r.state.Sealed != nil {
		receipt := *r.state.Sealed
		copy.state.Sealed = &receipt
	}
	return &copy
}

func decodeLedgerState(data string) (ledgerState, error) {
	var stored struct {
		UnitsUsed          *int64
		CostMicros         *int64
		ReservedUnits      *int64
		ReservedCostMicros *int64
		Pending            *int64 `json:"pendingReservations"`
		Unbounded          *int64 `json:"pendingUnboundedReservations"`
		Sealed             *struct {
			Units      *int64
			CostMicros *int64
		} `json:"sealed"`
	}
	decoder := json.NewDecoder(bytes.NewBufferString(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&stored); err != nil {
		return ledgerState{}, fmt.Errorf("budget: invalid ledger state: %w", err)
	}
	if stored.UnitsUsed == nil || stored.CostMicros == nil || stored.ReservedUnits == nil || stored.ReservedCostMicros == nil {
		return ledgerState{}, fmt.Errorf("budget: ledger counters are missing")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return ledgerState{}, fmt.Errorf("budget: ledger state has trailing data")
	}
	s := ledgerState{State: State{UnitsUsed: *stored.UnitsUsed, CostMicros: *stored.CostMicros,
		ReservedUnits: *stored.ReservedUnits, ReservedCostMicros: *stored.ReservedCostMicros}, Pending: stored.Pending, Unbounded: stored.Unbounded}
	if stored.Sealed != nil {
		if stored.Sealed.Units == nil || stored.Sealed.CostMicros == nil {
			return ledgerState{}, fmt.Errorf("budget: seal receipt counters are missing")
		}
		s.Sealed = &TrustedUsage{Units: *stored.Sealed.Units, CostMicros: *stored.Sealed.CostMicros}
	}
	if err := validateLedgerState(s); err != nil {
		return ledgerState{}, err
	}
	return s, nil
}

func validateLedgerState(s ledgerState) error {
	if err := validateState(s.State); err != nil {
		return err
	}
	if s.Sealed != nil && (s.Sealed.Units < s.UnitsUsed+s.ReservedUnits || s.Sealed.CostMicros < s.CostMicros+s.ReservedCostMicros) {
		return fmt.Errorf("budget: sealed accounting is inconsistent")
	}
	if (s.Pending == nil) != (s.Unbounded == nil) {
		return fmt.Errorf("budget: reservation counts are incomplete")
	}
	if s.Pending != nil && (*s.Pending < 0 || *s.Unbounded < 0 || *s.Unbounded > *s.Pending ||
		(*s.Pending == 0 && (s.ReservedUnits != 0 || s.ReservedCostMicros != 0))) {
		return fmt.Errorf("budget: reservation counts are inconsistent")
	}
	if s.Sealed != nil && (s.Pending == nil || *s.Unbounded != 0) {
		return fmt.Errorf("budget: sealed reservation history is unavailable or unbounded")
	}
	return nil
}
