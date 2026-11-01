package budget

import (
	"context"
	"sync"
)

// requestSlot coordinates the calls one gateway holds against an account.
// Bounded calls run together while their reservations fit. A call that
// reserves all remaining capacity runs alone. changed is closed and replaced
// whenever a call ends, so waiting calls retry their reservations.
type requestSlot struct {
	users            int
	inFlight         int
	exclusive        bool
	exclusiveWaiting int
	changed          chan struct{}
}

// ReserveRequest admits one call and reserves its capacity. A bounded call
// passes the units and cost its request can use at most; it runs alongside
// other bounded calls while its reservation fits. A call that does not fit
// waits for another call to end and retries. When no other call is pending,
// it reserves all remaining capacity instead, as an unbounded call does, and
// is refused only when nothing remains. An unbounded call reserves all
// remaining capacity, so it waits for every call in flight to end, and calls
// that arrive after it wait for it. Under no limit at all, every call runs
// as a bounded one.
//
// The returned function ends the call; call it after the reservation settles.
// The ledger remains authoritative, including after a gateway restart.
//
// recovery, when set, lets the ledger settle the reservation if this gateway
// stops before it does.
func (m *Meter) ReserveRequest(
	ctx context.Context, units, costMicros int64, bounded bool, recovery *Recovery,
) (*Reservation, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if m == nil {
		return nil, func() {}, nil
	}
	// Without either limit nothing is reserved, so no call needs to run
	// alone (ADR 0051).
	bounded = bounded || m.limits.MaxUnits == 0 && m.limits.MaxCostMicros == 0
	slot := m.joinSlot()
	defer m.leaveSlot(slot)
	for bounded {
		changed, err := m.waitFor(ctx, slot, func() bool { return !slot.exclusive && slot.exclusiveWaiting == 0 })
		if err != nil {
			return nil, nil, err
		}
		slot.inFlight++
		m.tracker.requests.Unlock()
		reservation, err := m.reserve(ctx, units, costMicros, recovery)
		if err == nil {
			return reservation, m.endCall(slot, false), nil
		}
		m.tracker.requests.Lock()
		slot.inFlight--
		others := slot.inFlight > 0
		m.tracker.requests.Unlock()
		if !IsExhausted(err) {
			return nil, nil, err
		}
		select {
		case <-changed:
			continue // a call ended since this attempt began
		default:
		}
		if !others {
			break // nothing else will free capacity; reserve what remains
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	m.tracker.requests.Lock()
	slot.exclusiveWaiting++
	m.tracker.requests.Unlock()
	_, err := m.waitFor(ctx, slot, func() bool { return !slot.exclusive && slot.inFlight == 0 })
	if err != nil {
		m.tracker.requests.Lock()
	}
	slot.exclusiveWaiting--
	if err != nil {
		m.signal(slot)
		m.tracker.requests.Unlock()
		return nil, nil, err
	}
	slot.exclusive = true
	m.tracker.requests.Unlock()
	reservation, err := m.reserve(ctx, 0, 0, recovery)
	if err != nil {
		m.endCall(slot, true)()
		return nil, nil, err
	}
	return reservation, m.endCall(slot, true), nil
}

// waitFor returns with the tracker's request lock held once ready reports
// true, with the channel that the next ended call closes.
func (m *Meter) waitFor(ctx context.Context, slot *requestSlot, ready func() bool) (chan struct{}, error) {
	for {
		m.tracker.requests.Lock()
		if ready() {
			return slot.changed, nil
		}
		changed := slot.changed
		m.tracker.requests.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// signal wakes waiting calls. The caller holds the request lock.
func (m *Meter) signal(slot *requestSlot) {
	close(slot.changed)
	slot.changed = make(chan struct{})
}

// endCall returns the function that ends an admitted call. The call holds the
// slot until then, so the slot outlives the admission that created it.
func (m *Meter) endCall(slot *requestSlot, exclusive bool) func() {
	m.tracker.requests.Lock()
	slot.users++
	m.tracker.requests.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			m.tracker.requests.Lock()
			if exclusive {
				slot.exclusive = false
			} else {
				slot.inFlight--
			}
			m.signal(slot)
			m.tracker.requests.Unlock()
			m.leaveSlot(slot)
		})
	}
}

func (m *Meter) joinSlot() *requestSlot {
	t := m.tracker
	t.requests.Lock()
	defer t.requests.Unlock()
	if t.activeRequests == nil {
		t.activeRequests = make(map[string]*requestSlot)
	}
	slot := t.activeRequests[m.key]
	if slot == nil {
		slot = &requestSlot{changed: make(chan struct{})}
		t.activeRequests[m.key] = slot
	}
	slot.users++
	return slot
}

func (m *Meter) leaveSlot(slot *requestSlot) {
	t := m.tracker
	t.requests.Lock()
	defer t.requests.Unlock()
	slot.users--
	if slot.users == 0 {
		delete(t.activeRequests, m.key)
	}
}
