package deadline

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

func TestPassed(t *testing.T) {
	for _, tc := range []struct {
		name string
		d    Deadline
		now  time.Time
		want bool
	}{
		{"none never passes", None(), t0.Add(100 * 365 * 24 * time.Hour), false},
		{"zero time is none", At(time.Time{}), t0, false},
		{"before", At(t0), t0.Add(-time.Nanosecond), false},
		{"at the instant", At(t0), t0, true},
		{"after", At(t0), t0.Add(time.Nanosecond), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.d.Passed(tc.now); got != tc.want {
				t.Fatalf("Passed = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestEarliest(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []Deadline
		want Deadline
	}{
		{"empty", nil, None()},
		{"all none", []Deadline{None(), None()}, None()},
		{"none never wins", []Deadline{None(), At(t0.Add(time.Hour))}, At(t0.Add(time.Hour))},
		{"none last", []Deadline{At(t0.Add(time.Hour)), None()}, At(t0.Add(time.Hour))},
		{"earlier wins", []Deadline{At(t0.Add(2 * time.Hour)), At(t0.Add(time.Hour)), None()}, At(t0.Add(time.Hour))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Earliest(tc.in...); !got.Equal(tc.want) {
				t.Fatalf("Earliest = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWithinRemainingAndBound(t *testing.T) {
	if None().Within(t0, 24*time.Hour) {
		t.Fatal("no deadline fits within a cap")
	}
	if !At(t0.Add(time.Hour)).Within(t0, time.Hour) || At(t0.Add(time.Hour+1)).Within(t0, time.Hour) {
		t.Fatal("Within boundary")
	}
	if _, ok := None().Remaining(t0); ok {
		t.Fatal("no deadline has remaining time")
	}
	if left, ok := At(t0).Remaining(t0.Add(time.Minute)); !ok || left != 0 {
		t.Fatalf("Remaining past the deadline = %v, %v", left, ok)
	}
	if got := None().Bound(t0, 2*time.Minute); !got.Equal(t0.Add(2 * time.Minute)) {
		t.Fatalf("no deadline bounds by the limit, got %v", got)
	}
	if got := At(t0.Add(time.Minute)).Bound(t0, 2*time.Minute); !got.Equal(t0.Add(time.Minute)) {
		t.Fatalf("a nearer deadline bounds, got %v", got)
	}
}

func TestContext(t *testing.T) {
	ctx, cancel := None().Context(context.Background())
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("no deadline gave a context deadline")
	}
	if ctx.Err() != nil {
		t.Fatal("no deadline ended the context")
	}
	ctx, cancel = At(t0).Context(context.Background())
	defer cancel()
	if got, ok := ctx.Deadline(); !ok || !got.Equal(t0) {
		t.Fatalf("context deadline = %v, %v", got, ok)
	}
}

func TestParseAndString(t *testing.T) {
	if d, err := Parse(NoneArg); err != nil || d.Set() {
		t.Fatalf("Parse(none) = %v, %v", d, err)
	}
	d, err := Parse(At(t0).String())
	if err != nil || !d.Equal(At(t0)) {
		t.Fatalf("round trip = %v, %v", d, err)
	}
	for _, bad := range []string{"", "0001-01-01T00:00:00Z", "tomorrow"} {
		if _, err := Parse(bad); err == nil {
			t.Fatalf("Parse(%q) accepted", bad)
		}
	}
}

func TestParseMaxDurationAndAfter(t *testing.T) {
	for in, want := range map[string]time.Duration{"none": 0, "30m": 30 * time.Minute, "48h": 48 * time.Hour} {
		if got, err := ParseMaxDuration(in); err != nil || got != want {
			t.Fatalf("ParseMaxDuration(%q) = %v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "0s", "-1h", "forever"} {
		if _, err := ParseMaxDuration(bad); err == nil {
			t.Fatalf("ParseMaxDuration(%q) accepted", bad)
		}
	}
	if After(t0, 0).Set() {
		t.Fatal("no duration made a deadline")
	}
	if got := After(t0, time.Hour); !got.Equal(At(t0.Add(time.Hour))) {
		t.Fatalf("After = %v", got)
	}
}

func TestFits(t *testing.T) {
	if !None().Fits(t0, 0) || !At(t0.Add(48*time.Hour)).Fits(t0, 0) {
		t.Fatal("no cap admits any deadline")
	}
	if None().Fits(t0, time.Hour) {
		t.Fatal("a cap admitted no deadline")
	}
	if !At(t0.Add(time.Hour)).Fits(t0, time.Hour) || At(t0.Add(2*time.Hour)).Fits(t0, time.Hour) {
		t.Fatal("cap boundary")
	}
}

func TestJSON(t *testing.T) {
	type wire struct {
		Deadline Deadline `json:"deadline,omitzero"`
	}
	b, err := json.Marshal(wire{At(t0)})
	if err != nil || string(b) != `{"deadline":"2026-11-14T12:00:00Z"}` {
		t.Fatalf("set = %s, %v", b, err)
	}
	if b, _ = json.Marshal(wire{}); string(b) != `{}` {
		t.Fatalf("none = %s", b)
	}
	for _, in := range []string{`{}`, `{"deadline":null}`, `{"deadline":"0001-01-01T00:00:00Z"}`} {
		var w wire
		if err := json.Unmarshal([]byte(in), &w); err != nil || w.Deadline.Set() {
			t.Fatalf("%s = %v, %v", in, w.Deadline, err)
		}
	}
}

// A host that slept past the deadline ends the session at the next check,
// although the monotonic clock says the deadline is still far off.
func TestDoneFollowsTheWallClockAfterSleep(t *testing.T) {
	start := time.Now().Round(0)
	d := At(start.Add(time.Hour))
	var slept atomic.Bool
	clock := func() time.Time {
		if slept.Load() {
			return d.Time().Add(time.Minute)
		}
		return start
	}
	reached := d.done(nil, 10*time.Millisecond, clock)
	select {
	case <-reached:
		t.Fatal("reached the deadline before it")
	case <-time.After(50 * time.Millisecond):
	}
	slept.Store(true)
	select {
	case <-reached:
	case <-time.After(time.Second):
		t.Fatal("the deadline passed on the wall clock but was not reached")
	}
}

func TestDoneStopsWithoutReaching(t *testing.T) {
	stop := make(chan struct{})
	reached := At(time.Now().Add(time.Hour)).Done(stop)
	close(stop)
	select {
	case <-reached:
		t.Fatal("a stopped wait reached its deadline")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestDoneWithoutADeadlineNeverCloses(t *testing.T) {
	select {
	case <-None().Done(nil):
		t.Fatal("no deadline was reached")
	case <-time.After(30 * time.Millisecond):
	}
}
