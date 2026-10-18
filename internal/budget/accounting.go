package budget

import (
	"fmt"
	"math"
)

func validateState(s State) error {
	if s.UnitsUsed < 0 || s.CostMicros < 0 || s.ReservedUnits < 0 || s.ReservedCostMicros < 0 ||
		s.UnitsUsed > math.MaxInt64-s.ReservedUnits || s.CostMicros > math.MaxInt64-s.ReservedCostMicros {
		return fmt.Errorf("budget: negative or overflowing ledger state")
	}
	return nil
}

func reserveState(s *State, req UsageReservation) (*LedgerReservation, error) {
	if req.MaxUnits < 0 || req.MaxCostMicros < 0 || req.Units < 0 || req.CostMicros < 0 {
		return nil, fmt.Errorf("budget: budget reservation values must not be negative")
	}
	units, cost := req.Units, req.CostMicros
	if req.MaxUnits > 0 {
		available := req.MaxUnits - s.UnitsUsed - s.ReservedUnits
		if available <= 0 || (units > 0 && units > available) {
			return nil, &ErrExhausted{UnitsUsed: s.UnitsUsed + s.ReservedUnits, MaxUnits: req.MaxUnits}
		}
		if units == 0 {
			units = available
		}
	}
	if req.MaxCostMicros > 0 {
		available := req.MaxCostMicros - s.CostMicros - s.ReservedCostMicros
		if available <= 0 || (cost > 0 && cost > available) {
			return nil, fmt.Errorf("budget: cost budget exhausted (%d of %d micros used)", s.CostMicros+s.ReservedCostMicros, req.MaxCostMicros)
		}
		if cost == 0 {
			cost = available
		}
	}
	if units > math.MaxInt64-s.UnitsUsed-s.ReservedUnits || cost > math.MaxInt64-s.CostMicros-s.ReservedCostMicros {
		return nil, fmt.Errorf("budget: reservation accounting overflows")
	}
	s.ReservedUnits += units
	s.ReservedCostMicros += cost
	return &LedgerReservation{runID: AccountKey(""), units: units, costMicros: cost, maxUnits: req.MaxUnits, maxCostMicros: req.MaxCostMicros}, nil
}

// settleState is shared by all storage adapters. Invalid usage cannot free
// capacity that may already have been consumed by the provider.
//
// Trusted usage above the reservation or the allowance is still recorded,
// capped at the allowance, and reported as exceeded. The provider has already
// done that work, so keeping only the reservation would let a caller repeat
// an oversized request indefinitely while each attempt charged a small bound.
func settleState(s *State, r *LedgerReservation, usage TrustedUsage) (exceeded bool, err error) {
	if r.settled {
		return false, fmt.Errorf("budget: budget reservation already settled")
	}
	if usage.Units < 0 || usage.CostMicros < 0 {
		return false, fmt.Errorf("budget: settled usage must not be negative")
	}
	if s.ReservedUnits < r.units || s.ReservedCostMicros < r.costMicros {
		return false, fmt.Errorf("budget: reservation state is inconsistent")
	}
	if usage.Units > math.MaxInt64-s.UnitsUsed-(s.ReservedUnits-r.units) ||
		usage.CostMicros > math.MaxInt64-s.CostMicros-(s.ReservedCostMicros-r.costMicros) {
		return false, fmt.Errorf("budget: settled accounting overflows")
	}
	units, unitsExceeded := cappedCharge(usage.Units, r.units, r.maxUnits, s.UnitsUsed)
	cost, costExceeded := cappedCharge(usage.CostMicros, r.costMicros, r.maxCostMicros, s.CostMicros)
	s.ReservedUnits -= r.units
	s.ReservedCostMicros -= r.costMicros
	s.UnitsUsed += units
	s.CostMicros += cost
	return unitsExceeded || costExceeded, nil
}

// cappedCharge returns the amount to record for used against one bounded
// dimension. Usage within the reservation and the allowance is recorded as is.
// Usage beyond either is recorded up to the allowance left after earlier
// settlements, so the account reaches its limit and refuses later requests.
// The charge is never less than the reservation, which was admitted within
// the allowance, unless the usage itself is smaller.
func cappedCharge(used, reserved, allowance, settled int64) (int64, bool) {
	if allowance <= 0 || (used <= reserved && settled+used <= allowance) {
		return used, false
	}
	return min(used, max(allowance-settled, reserved)), true
}
