// Package deadline is the one rule for an execution's deadline. Whoever
// starts work sets it (ADR 0056), so an execution may have none: the zero
// value. Every check of whether an execution's deadline has passed goes
// through this package, so a missing deadline never counts as expired and a
// set one is reached at the same instant everywhere: at the deadline, not
// after it.
package deadline

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

// NoneArg is the argument, flag and configuration value that asks for no
// deadline.
const NoneArg = "none"

// Deadline is an execution's deadline, or none. The zero value is none.
type Deadline struct{ at time.Time }

// At is the deadline at t. The zero time is no deadline.
func At(t time.Time) Deadline {
	if t.IsZero() {
		return Deadline{}
	}
	return Deadline{at: t}
}

// None is no deadline.
func None() Deadline { return Deadline{} }

// Set reports whether there is a deadline.
func (d Deadline) Set() bool { return !d.at.IsZero() }

// Time is the deadline's instant, or the zero time when there is none.
func (d Deadline) Time() time.Time { return d.at }

// Passed reports whether now is at or after a set deadline. Without a
// deadline it never passes.
func (d Deadline) Passed(now time.Time) bool { return d.Set() && !now.Before(d.at) }

// Remaining is the time left before a set deadline, never negative. ok is
// false when there is no deadline.
func (d Deadline) Remaining(now time.Time) (left time.Duration, ok bool) {
	if !d.Set() {
		return 0, false
	}
	return max(d.at.Sub(now), 0), true
}

// Within reports whether a set deadline is no later than limit after now.
// No deadline is within no limit, so a cap denies it.
func (d Deadline) Within(now time.Time, limit time.Duration) bool {
	return d.Set() && !d.at.After(now.Add(limit))
}

// Fits reports whether the deadline fits a cap of limit from now: any
// deadline, none included, fits no cap (limit 0); under a cap a deadline must
// be set and Within it.
func (d Deadline) Fits(now time.Time, limit time.Duration) bool {
	return limit == 0 || d.Within(now, limit)
}

// Equal reports whether both are none or both are the same instant.
func (d Deadline) Equal(o Deadline) bool { return d.at.Equal(o.at) }

// Earliest is the earliest set deadline. No deadline never wins; the result
// is none only when every argument is none.
func Earliest(ds ...Deadline) Deadline {
	var out Deadline
	for _, d := range ds {
		if d.Set() && (!out.Set() || d.at.Before(out.at)) {
			out = d
		}
	}
	return out
}

// Bound is the earlier of a set deadline and now+limit: when to stop waiting
// for something that must finish by the deadline and in any case within
// limit. It is never the zero time.
func (d Deadline) Bound(now time.Time, limit time.Duration) time.Time {
	return Earliest(d, At(now.Add(limit))).at
}

// Context is a child of parent that ends at a set deadline, or only with
// parent when there is none.
func (d Deadline) Context(parent context.Context) (context.Context, context.CancelFunc) {
	if d.Set() {
		return context.WithDeadline(parent, d.at)
	}
	return context.WithCancel(parent)
}

// String is the RFC 3339 instant, or "none".
func (d Deadline) String() string {
	if !d.Set() {
		return NoneArg
	}
	return d.at.UTC().Format(time.RFC3339Nano)
}

// Parse reads an argument written by String: an RFC 3339 instant or "none".
func Parse(s string) (Deadline, error) {
	if s == NoneArg {
		return Deadline{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.IsZero() {
		return Deadline{}, errors.New("deadline: want an RFC 3339 time or none")
	}
	return At(t), nil
}

// ParseMaxDuration reads a maximum duration as configured or passed to
// --max-duration: "none" for no deadline (0), or a positive Go duration.
func ParseMaxDuration(s string) (time.Duration, error) {
	if s == NoneArg {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, errors.New(`want a positive duration such as "30m" or "8h", or "none"`)
	}
	return d, nil
}

// After is the deadline d after now, or none when d is 0.
func After(now time.Time, d time.Duration) Deadline {
	if d == 0 {
		return Deadline{}
	}
	return At(now.Add(d))
}

// MarshalJSON writes a set deadline as an RFC 3339 time and none as null.
// A struct field omits none with omitzero.
func (d Deadline) MarshalJSON() ([]byte, error) {
	if !d.Set() {
		return []byte("null"), nil
	}
	return json.Marshal(d.at)
}

// UnmarshalJSON reads an RFC 3339 time; null and the zero time are none.
func (d *Deadline) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*d = Deadline{}
		return nil
	}
	var t time.Time
	if err := json.Unmarshal(b, &t); err != nil {
		return err
	}
	*d = At(t)
	return nil
}

// IsZero reports no deadline, for omitzero.
func (d Deadline) IsZero() bool { return !d.Set() }

// wallClock reads the time without its monotonic reading, so a comparison
// uses the wall clock.
func wallClock() time.Time { return time.Now().Round(0) }

// Done returns a channel that closes once the wall clock reaches a set
// deadline. It never closes if stop closes first or there is no deadline. It
// reads the wall clock every second rather than waiting for the remaining
// duration, because a timer counts monotonic time, which stands still while
// the host sleeps: in a Docker Desktop VM, a Mac that slept for half an hour
// would otherwise end a session half an hour late.
func (d Deadline) Done(stop <-chan struct{}) <-chan struct{} {
	return d.done(stop, time.Second, wallClock)
}

// done is Done reading clock every interval.
func (d Deadline) done(stop <-chan struct{}, every time.Duration, clock func() time.Time) <-chan struct{} {
	reached := make(chan struct{})
	if !d.Set() {
		return reached
	}
	go func() {
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for !d.Passed(clock()) {
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
		close(reached)
	}()
	return reached
}
