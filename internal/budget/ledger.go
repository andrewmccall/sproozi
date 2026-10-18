package budget

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

var ErrAccountNotFound = errors.New("budget account not found")

// Store owns atomic durable records. Update may repeat the callback after CAS
// conflicts; it publishes the record only when the callback succeeds. Absent
// records start at NewRecord. Read never turns failures into an absent record.
type Store interface {
	Read(context.Context, AccountKey) (*Record, error)
	Update(context.Context, AccountKey, func(*Record, bool) error) error
}

type ledger struct{ store Store }

func NewLedger(store Store) Ledger { return &ledger{store: store} }

// MemoryLedger uses the same admission/seal rules as durable storage, with one
// mutex-backed record owner. It is injectable for tests, not persistent storage.
type MemoryLedger struct{ *ledger }

type memoryStore struct {
	mu      sync.Mutex
	records map[AccountKey]*Record
}

func NewMemoryLedger() *MemoryLedger {
	return &MemoryLedger{ledger: &ledger{store: &memoryStore{records: make(map[AccountKey]*Record)}}}
}

func (s *memoryStore) Read(_ context.Context, key AccountKey) (*Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[key]
	if !ok {
		return nil, ErrAccountNotFound
	}
	return cloneRecord(record), nil
}

func (s *memoryStore) Update(_ context.Context, key AccountKey, mutate func(*Record, bool) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, exists := s.records[key]
	next := NewRecord()
	if exists {
		next = cloneRecord(current)
	}
	if err := mutate(next, exists); err != nil {
		return err
	}
	if err := validateLedgerState(next.state); err != nil {
		return err
	}
	s.records[key] = next
	return nil
}

func (l *ledger) Reserve(ctx context.Context, runID AccountKey, req UsageReservation) (*LedgerReservation, error) {
	var result *LedgerReservation
	err := l.store.Update(ctx, runID, func(record *Record, exists bool) error {
		if req.RequireExisting && !exists {
			return fmt.Errorf("budget: initialized account is unavailable")
		}
		s := &record.state
		if s.Sealed != nil {
			return fmt.Errorf("budget: account is sealed")
		}
		reservation, err := reserveState(&s.State, req)
		if err != nil {
			return err
		}
		result = reservation
		if s.Pending != nil {
			*s.Pending++
			if reservation.unbounded() {
				*s.Unbounded++
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if result != nil {
		result.runID = runID
	}
	return result, err
}

func (l *ledger) Settle(ctx context.Context, r *LedgerReservation, usage TrustedUsage) error {
	if r == nil {
		return fmt.Errorf("budget: nil budget reservation")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	var exceeded bool
	err := l.store.Update(ctx, r.runID, func(record *Record, _ bool) error {
		s := &record.state
		var err error
		if exceeded, err = settleState(&s.State, r, usage); err != nil {
			return err
		}
		if s.Pending != nil {
			*s.Pending--
			if r.unbounded() {
				*s.Unbounded--
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	r.settled = true
	if exceeded {
		return ErrUsageExceeded
	}
	return nil
}

func (l *ledger) Release(ctx context.Context, r *LedgerReservation) error {
	return l.Settle(ctx, r, TrustedUsage{})
}

func (l *ledger) State(ctx context.Context, runID AccountKey) (State, error) {
	record, err := l.store.Read(ctx, runID)
	if err != nil {
		return State{}, err
	}
	return record.state.State, nil
}

func (l *ledger) Initialize(ctx context.Context, key AccountKey) error {
	return l.store.Update(ctx, key, func(record *Record, _ bool) error {
		s := &record.state
		if s.Pending == nil {
			return fmt.Errorf("budget: legacy reservation history is unavailable")
		}
		if s.Sealed != nil {
			return fmt.Errorf("budget: account is sealed")
		}
		return nil
	})
}

func (l *ledger) Seal(ctx context.Context, runID AccountKey) (TrustedUsage, error) {
	var receipt TrustedUsage
	err := l.store.Update(ctx, runID, func(record *Record, exists bool) error {
		s := &record.state
		if !exists {
			return fmt.Errorf("budget: initialized account is unavailable")
		}
		if s.Pending == nil {
			return fmt.Errorf("budget: legacy reservation history is unavailable for sealing")
		}
		if *s.Unbounded > 0 {
			return fmt.Errorf("budget: pending usage has no finite bound")
		}
		if s.Sealed == nil {
			value := TrustedUsage{Units: s.UnitsUsed + s.ReservedUnits, CostMicros: s.CostMicros + s.ReservedCostMicros}
			s.Sealed = &value
		}
		receipt = *s.Sealed
		return nil
	})
	if err != nil {
		return TrustedUsage{}, err
	}
	return receipt, nil
}
