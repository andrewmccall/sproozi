package budget

import "context"

type requestSlot struct {
	occupied chan struct{}
	users    int
}

// AcquireRequest serializes calls sharing an allowance. A request which reserves
// all remaining capacity must settle before the next call can reserve it.
// The ledger remains authoritative, including after a gateway restart.
func (m *Meter) AcquireRequest(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m == nil {
		return func() {}, nil
	}
	t := m.tracker
	t.requests.Lock()
	if t.activeRequests == nil {
		t.activeRequests = make(map[string]*requestSlot)
	}
	slot := t.activeRequests[m.key]
	if slot == nil {
		slot = &requestSlot{occupied: make(chan struct{}, 1)}
		t.activeRequests[m.key] = slot
	}
	slot.users++
	t.requests.Unlock()
	drop := func() {
		t.requests.Lock()
		defer t.requests.Unlock()
		slot.users--
		if slot.users == 0 {
			delete(t.activeRequests, m.key)
		}
	}
	select {
	case slot.occupied <- struct{}{}:
		return func() { <-slot.occupied; drop() }, nil
	case <-ctx.Done():
		drop()
		return nil, ctx.Err()
	}
}
