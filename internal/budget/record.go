package budget

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
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
	// Leases are the recoverable pending reservations, by id. Omitted when
	// there are none, so other records keep their stored format.
	Leases map[string]lease `json:"leases,omitempty"`
}

// accountPhase is where an account is in its lifecycle. The ledger's
// operations decide by phase rather than by reading the record's fields.
type accountPhase int

const (
	// accountAbsent has no stored record. Only Initialize and a Reserve that
	// does not require an existing account create one; nothing recreates a
	// deleted account.
	accountAbsent accountPhase = iota
	// accountLegacy predates pending-reservation counts, so its reservation
	// history is unknown, not zero. It keeps ordinary accounting but can
	// never seal.
	accountLegacy
	// accountOpen admits reservations and can seal.
	accountOpen
	// accountUnbounded is open with a pending reservation that has no finite
	// bound. It cannot seal until that reservation settles.
	accountUnbounded
	// accountSealed refuses reservations. Its receipt already holds every
	// pending reservation in full, so each settles at most its reservation.
	accountSealed
)

// phase reports the account's phase; exists is whether its record is stored.
func (s ledgerState) phase(exists bool) accountPhase {
	switch {
	case !exists:
		return accountAbsent
	case s.Sealed != nil:
		return accountSealed
	case s.Pending == nil:
		return accountLegacy
	case *s.Unbounded > 0:
		return accountUnbounded
	default:
		return accountOpen
	}
}

// settled reports whether a stored account has no reservation outstanding:
// no reserved units or cost, no pending count and no lease. A legacy account
// is never settled, since its pending count is unknown.
func (s ledgerState) settled() bool {
	switch s.phase(true) {
	case accountOpen, accountSealed:
		return *s.Pending == 0 && s.ReservedUnits == 0 && s.ReservedCostMicros == 0 && len(s.Leases) == 0
	default:
		return false
	}
}

// settle settles reservation r at usage under the account's phase, and
// reports whether usage exceeded what could be charged. An open account
// charges usage above the reservation up to the allowance; a sealed account
// charges at most the reservation, which its immutable receipt already holds.
func (s *ledgerState) settle(r *LedgerReservation, usage TrustedUsage) (exceeded bool, err error) {
	charge := cappedCharge
	if s.phase(true) == accountSealed {
		charge = reservedCharge
	}
	if exceeded, err = settleState(&s.State, r, usage, charge); err != nil {
		return false, err
	}
	if s.phase(true) != accountLegacy {
		*s.Pending--
		if r.unbounded() {
			*s.Unbounded--
		}
	}
	return exceeded, nil
}

// Existing adapts mutate to Store.Update for an operation on an account that
// must already exist, such as a settlement or a recovery: an absent account
// returns ErrAccountNotFound rather than being recreated as an empty record.
func Existing(mutate func(*Record) error) func(*Record, bool) error {
	return func(record *Record, exists bool) error {
		if record.state.phase(exists) == accountAbsent {
			return ErrAccountNotFound
		}
		return mutate(record)
	}
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

// Usage is the settled usage and whether the account is sealed, for display
// beside the encoded record. It reads the record and changes nothing.
func (r *Record) Usage() (units, costMicros int64, finalized bool) {
	return r.state.UnitsUsed, r.state.CostMicros, r.state.phase(true) == accountSealed
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
	copy.state.Leases = maps.Clone(r.state.Leases)
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
		Leases map[string]lease `json:"leases"`
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
		ReservedUnits: *stored.ReservedUnits, ReservedCostMicros: *stored.ReservedCostMicros}, Pending: stored.Pending, Unbounded: stored.Unbounded,
		Leases: stored.Leases}
	if len(s.Leases) == 0 {
		s.Leases = nil
	}
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
	var leasedUnits, leasedCost int64
	for _, l := range s.Leases {
		if l.Units < 0 || l.CostMicros < 0 || l.ChargeUnits < 0 || l.ChargeCost < 0 ||
			l.Units > s.ReservedUnits-leasedUnits || l.CostMicros > s.ReservedCostMicros-leasedCost {
			return fmt.Errorf("budget: reservation leases are inconsistent")
		}
		leasedUnits += l.Units
		leasedCost += l.CostMicros
	}
	if s.Pending != nil && int64(len(s.Leases)) > *s.Pending {
		return fmt.Errorf("budget: reservation leases are inconsistent")
	}
	if s.Sealed != nil && (s.Pending == nil || *s.Unbounded != 0) {
		return fmt.Errorf("budget: sealed reservation history is unavailable or unbounded")
	}
	return nil
}
