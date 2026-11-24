package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
	"github.com/andrewmccall/sproozi/internal/testutil"
)

// Codex's real quit, typed into a harness that reads its terminal, ends the
// harness within the supervisor's quit wait: finishTurn reports a clean
// native exit, not a forced stop. The lifecycle record gives a real quit its
// own bound, so this is where the quit wait itself is asserted. The wait is
// a real deadline; a machine too slow to quit within it starts again with
// the wait doubled, so only the test's length depends on the machine.
func TestCodexQuitEndsWithinTheQuitWait(t *testing.T) {
	previous := quitWait
	t.Cleanup(func() { quitWait = previous })
	adapter := nativeAdapters[harness.NativeCodex]
	testutil.PassDeadline(t, 600*time.Millisecond, func(ahead time.Duration) (time.Time, error) {
		quitWait = ahead
		n := startNative(t, recordedHarness, time.Time{})
		for !strings.Contains(string(n.Tail(64)), "ready") {
			select {
			case <-n.Done():
				return time.Now().Add(ahead), errors.New("the harness exited before it was ready")
			case <-time.After(5 * time.Millisecond):
			}
		}
		n.GateInput()
		s := &server{config: config{Harness: harness.NativeCodex}}
		until := time.Now().Add(ahead)
		exit, forced, err := s.finishTurn(adapter, n)
		switch {
		case err != nil:
			return until, err
		case forced != "":
			return until, errors.New(forced)
		case exit.Reason != nativepty.NativeExit || exit.Code != 0:
			return until, fmt.Errorf("the quit ended the harness with %+v", exit)
		}
		return until, nil
	})
}
