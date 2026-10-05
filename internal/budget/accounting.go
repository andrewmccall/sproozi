package budget

import "fmt"

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
	s.ReservedUnits += units
	s.ReservedCostMicros += cost
	return &LedgerReservation{runID: AccountKey(""), units: units, costMicros: cost, maxUnits: req.MaxUnits, maxCostMicros: req.MaxCostMicros}, nil
}

// settleState is shared by all storage adapters. Invalid usage cannot free
// capacity that may already have been consumed by the provider.
func settleState(s *State, r *LedgerReservation, usage TrustedUsage) error {
	if r.settled {
		return fmt.Errorf("budget: budget reservation already settled")
	}
	if usage.Units < 0 || usage.CostMicros < 0 {
		return fmt.Errorf("budget: settled usage must not be negative")
	}
	if s.ReservedUnits < r.units || s.ReservedCostMicros < r.costMicros {
		return fmt.Errorf("budget: reservation state is inconsistent")
	}
	if (r.maxUnits > 0 && usage.Units > r.units) || (r.maxCostMicros > 0 && usage.CostMicros > r.costMicros) || (r.maxUnits > 0 && s.UnitsUsed+usage.Units > r.maxUnits) || (r.maxCostMicros > 0 && s.CostMicros+usage.CostMicros > r.maxCostMicros) {
		return fmt.Errorf("budget: settled usage exceeds reservation or budget")
	}
	s.ReservedUnits -= r.units
	s.ReservedCostMicros -= r.costMicros
	s.UnitsUsed += usage.Units
	s.CostMicros += usage.CostMicros
	return nil
}
