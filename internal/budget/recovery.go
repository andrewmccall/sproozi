package budget

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"
	"time"
)

// Recovery makes a reservation recoverable after its gateway stops without
// settling it, for example when it is killed or runs out of memory. The ledger
// keeps the reservation as a lease. Once ExpiresAt has passed, the periodic
// sweep, or the next reservation or seal of the account, settles the lease at
// the given charge, capped at what it reserved. A restart therefore never
// refunds an unknown reservation, and never holds it for good.
type Recovery struct {
	// ExpiresAt is after the call's latest possible settlement: its deadline
	// plus a margin.
	ExpiresAt time.Time
	// Units and CostMicros are an estimate of an abandoned call's usage.
	Units, CostMicros int64
	// Model marks a model call's reservation, for the recovery log line.
	Model bool
}

// RecoveryMargin follows a reservation's deadline before its lease expires:
// past its settlement and any clock skew.
const RecoveryMargin = 5 * time.Minute

// requestRecovery is the lease for a reservation that is not a model call,
// such as a GitHub, package, Kubernetes, egress or MCP request. Its gateway
// charged it in full when it could not settle, so recovery does too. It
// expires after the request's deadline.
func requestRecovery(ctx context.Context, units, costMicros int64, limits UsageReservation) *Recovery {
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(time.Hour)
	}
	full := func(amount, limit int64) int64 {
		if limit > 0 {
			return math.MaxInt64 // capped at what it reserved
		}
		return amount
	}
	return &Recovery{ExpiresAt: deadline.Add(RecoveryMargin), Units: full(units, limits.MaxUnits),
		CostMicros: full(costMicros, limits.MaxCostMicros)}
}

// ErrTooManyPending refuses a reservation while the account already holds
// the most recoverable pending reservations a record keeps. It is temporary:
// the reservation fits once a pending call settles.
var ErrTooManyPending = errors.New("too many model calls are pending on this budget; retry when one finishes")

// ErrAlreadySettled refuses a second settlement of one reservation.
var ErrAlreadySettled = errors.New("budget: reservation already settled")

// ErrReservationRecovered reports that a reservation's lease expired and was
// settled at its recovery charge before its gateway settled it.
var ErrReservationRecovered = errors.New("budget: reservation expired and was settled at its recovery charge")

// Recovery log fields: an abandoned model call is charged an estimate.
const (
	recoveryEndReason   = "gateway_restart"
	recoveryUsageSource = "estimated"
	maxLeases           = 64
)

// lease is the durable record of one recoverable pending reservation.
type lease struct {
	Units         int64 `json:"units"`
	CostMicros    int64 `json:"costMicros"`
	MaxUnits      int64 `json:"maxUnits"`
	MaxCostMicros int64 `json:"maxCostMicros"`
	ChargeUnits   int64 `json:"chargeUnits"`
	ChargeCost    int64 `json:"chargeCostMicros"`
	ExpiresAt     int64 `json:"expiresAt"`
	Model         bool  `json:"model,omitempty"`
}

func newLeaseID() (string, error) {
	var id [12]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", fmt.Errorf("budget: reservation lease id: %w", err)
	}
	return hex.EncodeToString(id[:]), nil
}

// addLease records reservation r as recoverable.
func addLease(s *ledgerState, r *LedgerReservation, recovery *Recovery) error {
	if len(s.Leases) >= maxLeases {
		return ErrTooManyPending
	}
	id, err := newLeaseID()
	if err != nil {
		return err
	}
	if s.Leases == nil {
		s.Leases = map[string]lease{}
	}
	s.Leases[id] = lease{Units: r.units, CostMicros: r.costMicros, MaxUnits: r.maxUnits, MaxCostMicros: r.maxCostMicros,
		ChargeUnits: max(recovery.Units, 0), ChargeCost: max(recovery.CostMicros, 0), ExpiresAt: recovery.ExpiresAt.Unix(),
		Model: recovery.Model}
	r.lease = id
	return nil
}

// takeLease removes reservation r's lease before it settles. A lease that is
// gone was recovered, so the reservation must not settle again.
func takeLease(s *ledgerState, r *LedgerReservation) error {
	if r.lease == "" {
		return nil
	}
	if _, held := s.Leases[r.lease]; !held {
		return ErrReservationRecovered
	}
	delete(s.Leases, r.lease)
	if len(s.Leases) == 0 {
		s.Leases = nil
	}
	return nil
}

// Recovered logs a committed recovery. A store may repeat or discard its
// update, so callers log only once the update that recovered has committed.
type Recovered func()

func (r Recovered) log() {
	if r != nil {
		r()
	}
}

// recoverExpired settles every lease past its expiry at its recovery charge,
// capped at its reservation, and returns the log of what it settled.
func recoverExpired(s *ledgerState, key AccountKey, now time.Time) (Recovered, error) {
	var lines []func()
	for _, id := range slices.Sorted(maps.Keys(s.Leases)) {
		l := s.Leases[id]
		if now.Unix() < l.ExpiresAt {
			continue
		}
		r := &LedgerReservation{units: l.Units, costMicros: l.CostMicros, maxUnits: l.MaxUnits, maxCostMicros: l.MaxCostMicros}
		units, cost := l.ChargeUnits, l.ChargeCost
		if l.MaxUnits > 0 {
			units = min(units, l.Units)
		}
		if l.MaxCostMicros > 0 {
			cost = min(cost, l.CostMicros)
		}
		if _, err := settleState(&s.State, r, TrustedUsage{Units: units, CostMicros: cost}); err != nil {
			return nil, err
		}
		if s.Pending != nil {
			*s.Pending--
			if r.unbounded() {
				*s.Unbounded--
			}
		}
		delete(s.Leases, id)
		message := "Settled abandoned budget reservation"
		if l.Model {
			message = "Settled abandoned model reservation"
		}
		lines = append(lines, func() {
			slog.Default().Info(message, "account", string(key),
				"endReason", recoveryEndReason, "usageSource", recoveryUsageSource, "units", units, "costMicros", cost)
		})
	}
	if len(s.Leases) == 0 {
		s.Leases = nil
	}
	return func() {
		for _, line := range lines {
			line()
		}
	}, nil
}

// expiredLease reports whether a record holds a lease past its expiry.
func expiredLease(s ledgerState, now time.Time) bool {
	for _, l := range s.Leases {
		if now.Unix() >= l.ExpiresAt {
			return true
		}
	}
	return false
}

// RecoverExpired settles a record's expired leases, for a sweep that finds
// the record by its storage name rather than its account key. With legacy
// set, it also charges in full every pending reservation no lease covers:
// every current reservation carries a lease, so one without a lease can only
// have been left by a gateway from before leases.
//
// The returned Recovered logs the settlements; call it only once the update
// that holds them has committed.
func RecoverExpired(r *Record, account string, now time.Time, legacy bool) (Recovered, error) {
	logExpired, err := recoverExpired(&r.state, AccountKey(account), now)
	if err != nil {
		return nil, err
	}
	var logLegacy Recovered
	if legacy {
		logLegacy = settleUnleased(&r.state, account)
	}
	return func() { logExpired.log(); logLegacy.log() }, nil
}

// NeedsRecovery reports whether an encoded record holds an expired lease, or
// with legacy set any pending reservation without a lease, so a sweep writes
// only the records it must change.
func NeedsRecovery(data string, now time.Time, legacy bool) (bool, error) {
	r, err := DecodeRecord(data)
	if err != nil {
		return false, err
	}
	if expiredLease(r.state, now) {
		return true, nil
	}
	units, cost, count := unleased(r.state)
	return legacy && (units > 0 || cost > 0 || count > 0), nil
}

// unleased is the pending amount and count that no lease covers.
func unleased(s ledgerState) (units, cost, count int64) {
	units, cost = s.ReservedUnits, s.ReservedCostMicros
	for _, l := range s.Leases {
		units -= l.Units
		cost -= l.CostMicros
	}
	if s.Pending != nil {
		count = *s.Pending - int64(len(s.Leases))
	}
	return units, cost, count
}

// settleUnleased charges the pending reservations no lease covers in full,
// as the gateway did before estimates: their process is gone.
func settleUnleased(s *ledgerState, account string) Recovered {
	units, cost, count := unleased(*s)
	if units <= 0 && cost <= 0 && count <= 0 {
		return nil
	}
	s.ReservedUnits -= units
	s.ReservedCostMicros -= cost
	s.UnitsUsed += units
	s.CostMicros += cost
	if s.Pending != nil {
		*s.Pending -= count
		var leasedUnbounded int64
		for _, l := range s.Leases {
			if l.MaxUnits == 0 || l.MaxCostMicros == 0 {
				leasedUnbounded++
			}
		}
		*s.Unbounded = leasedUnbounded
	}
	return func() {
		slog.Default().Info("Settled legacy budget reservation", "account", account,
			"endReason", recoveryEndReason, "usageSource", "reservation", "reservations", count,
			"units", units, "costMicros", cost)
	}
}

// Settled reports whether a stored ledger has no reservation outstanding: no
// reserved units or cost, no pending count and no lease. Only a settled ledger
// may be removed once its run is gone; its usage is final.
func Settled(data string) (bool, error) {
	r, err := DecodeRecord(data)
	if err != nil {
		return false, err
	}
	s := r.state
	// A missing pending count means legacy history is unknown, not zero.
	noPending := s.Pending != nil && *s.Pending == 0 && (s.Unbounded == nil || *s.Unbounded == 0)
	return noPending && s.ReservedUnits == 0 && s.ReservedCostMicros == 0 && len(s.Leases) == 0, nil
}
