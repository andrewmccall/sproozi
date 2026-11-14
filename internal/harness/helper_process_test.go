package harness_test

import (
	"context"
	"testing"
	"time"
)

// helperProcessContext bounds a launch helper by the test binary's own
// deadline rather than a fixed budget. The helpers start Python and run in
// parallel subtests, so on a loaded machine a fixed 10s budget killed them
// mid-run and the kill read as a launch failure. A hung helper still ends:
// at the go test -timeout deadline, less a margin to report it, or after
// two minutes when no deadline is set.
func helperProcessContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	budget := 2 * time.Minute
	if deadline, ok := t.Deadline(); ok {
		budget = time.Until(deadline) - 10*time.Second
	}
	return context.WithTimeout(t.Context(), max(budget, 10*time.Second))
}
