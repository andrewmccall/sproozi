package docker

import (
	"time"

	"github.com/andrewmccall/sproozi/internal/budget"
)

// SetClock replaces the clock o checks deadlines against.
func SetClock(o *Owner, now func() time.Time) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.now = now
}

// SaveRetained saves the retained grant id with its phase replaced and, when
// receipt is set, an empty receipt, through the owner's validating save.
func SaveRetained(o *Owner, id, phase string, receipt bool) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	r, err := o.load(id)
	if err != nil {
		return err
	}
	r.Phase = grantPhase(phase)
	if receipt {
		r.Receipt = map[string]budget.TrustedUsage{}
	}
	return o.save(r)
}
