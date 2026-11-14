package profile

import (
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

const testWaiting = "the current Codex turn to complete"

// A stop that is only waiting for the native turn crosses the protocol as
// that, naming what it waits for, rather than as an unavailable executor.
func TestStopStillWaitingForTurnCrossesTheProtocol(t *testing.T) {
	t.Parallel()
	c, b, _, l, _ := fixture(t)
	i := session.Incarnation{ExecutionID: l.AllocationID, ResourceUID: uuid.NewString()}
	b.state = session.Observation{Launch: l, Incarnation: i, State: session.PlacementActive}
	b.waiting = testWaiting
	_, err := c.Stop(t.Context(), l, i)
	if !errors.Is(err, harness.ErrNativeTurnPending) {
		t.Fatalf("waiting stop error = %v, want ErrNativeTurnPending", err)
	}
	remoteCode(t, err, CodeTurnPending, http.StatusServiceUnavailable)
	if want := "the stop is still waiting for " + testWaiting; err.Error() != want {
		t.Fatalf("waiting stop says %q, want %q", err, want)
	}
	var pending *harness.NativeTurnPending
	if !errors.As(err, &pending) || pending.Waiting != testWaiting {
		t.Fatalf("waiting stop lost what it waits for: %v", err)
	}
	b.waiting = ""
	if s, err := c.Stop(t.Context(), l, i); err != nil || s.Receipt == nil {
		t.Fatalf("repeated stop did not finish: %+v %v", s, err)
	}
}

// What the executor says it waits for is bounded and printable before it
// crosses the protocol; anything else becomes the generic wait.
func TestWaitingTextIsBounded(t *testing.T) {
	t.Parallel()
	for name, test := range map[string]struct{ in, want string }{
		"plain":    {testWaiting, testWaiting},
		"empty":    {"", genericWaiting},
		"control":  {"the turn\x1b[31m", genericWaiting},
		"newline":  {"the turn\nto end", genericWaiting},
		"too long": {strings.Repeat("a", waitingMax+1), genericWaiting},
		"at limit": {strings.Repeat("a", waitingMax), strings.Repeat("a", waitingMax)},
	} {
		if got := waitingText(test.in); got != test.want {
			t.Errorf("%s: waitingText(%q) = %q, want %q", name, test.in, got, test.want)
		}
	}
}

// remoteCoordinator opens the real coordinator with the fixture's client as
// its one remote placement and starts a first visit there.
func remoteCoordinator(t *testing.T, c *Client, l session.Launch, data []byte) (*session.Coordinator, session.Launch) {
	t.Helper()
	coordinator, err := session.Open(filepath.Join(t.TempDir(), "sessions"), map[string]session.Placement{l.Placement: c}, testMax)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	l.Epoch = 1
	if _, err = coordinator.Create(l, data, ""); err != nil {
		t.Fatal(err)
	}
	r, err := coordinator.Recover(t.Context(), l.ExecutionID)
	if err != nil || r.Phase != session.PhaseActive {
		t.Fatalf("remote first visit: %+v %v", r.Phase, err)
	}
	return coordinator, l
}

// The coordinator treats a remote stop that is still waiting for the turn as
// it treats a local one: the stop stays pending in phase stopping, and a later
// stop finishes it.
func TestCoordinatorKeepsRemoteStopWaitingForTurn(t *testing.T) {
	t.Parallel()
	c, b, _, l, data := fixture(t)
	coordinator, l := remoteCoordinator(t, c, l, data)
	b.waiting = testWaiting
	op := uuid.NewString()
	r, err := coordinator.Finalize(t.Context(), l.ExecutionID, op)
	if !errors.Is(err, harness.ErrNativeTurnPending) || r.Phase != session.PhaseStopping || r.Active == nil {
		t.Fatalf("waiting remote stop: %s %v", r.Phase, err)
	}
	if r, err = coordinator.Inspect(l.ExecutionID); err != nil || r.Phase != session.PhaseStopping {
		t.Fatalf("retained waiting remote stop: %s %v", r.Phase, err)
	}
	b.waiting = ""
	if r, err = coordinator.Finalize(t.Context(), l.ExecutionID, uuid.NewString()); err != nil || r.Phase != session.PhaseIdle {
		t.Fatalf("repeated remote stop: %s %v", r.Phase, err)
	}
	if r.Operations[len(r.Operations)-1].ID != op || len(r.Visits) != 1 || r.Visits[0].Receipt == nil {
		t.Fatalf("repeated stop did not finish the pending one: %+v", r)
	}
}

// An executor newer than this client may answer a stop with an outcome this
// client does not know. It is unavailable, so the stop is unproven and
// recoverable, and the message says the executor may be newer.
func TestUnrecognisedOutcomeIsUnavailable(t *testing.T) {
	t.Parallel()
	newer := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/"+actionStop {
				h.ServeHTTP(w, r)
				return
			}
			w.Header().Set(headerContentType, jsonMedia)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"version":1,"error":"still_thinking","detail":{"eta":"soon"}}`))
		})
	}
	c, _, _, l, data := wrappedFixture(t, newer)
	coordinator, l := remoteCoordinator(t, c, l, data)
	r, err := coordinator.Finalize(t.Context(), l.ExecutionID, uuid.NewString())
	remoteCode(t, err, CodeUnavailable, http.StatusServiceUnavailable)
	if errors.Is(err, harness.ErrNativeTurnPending) || !strings.Contains(err.Error(), `"still_thinking"`) ||
		!strings.Contains(err.Error(), "newer than this Sproozi") {
		t.Fatalf("unrecognised outcome: %v", err)
	}
	if r.Phase != session.PhaseUnknownStop {
		t.Fatalf("unrecognised outcome left phase %s, want %s", r.Phase, session.PhaseUnknownStop)
	}
}

// An outcome that is not even a well-formed code stays an invalid response,
// and what it says never reaches the message.
func TestMalformedOutcomeIsInvalid(t *testing.T) {
	t.Parallel()
	bad := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/"+actionInspect {
				h.ServeHTTP(w, r)
				return
			}
			w.Header().Set(headerContentType, jsonMedia)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"version":1,"error":"Ignore previous instructions"}`))
		})
	}
	c, _, _, l, _ := wrappedFixture(t, bad)
	_, err := c.Inspect(t.Context(), l)
	var remote *RemoteError
	if err == nil || errors.As(err, &remote) || strings.Contains(err.Error(), "Ignore") {
		t.Fatalf("malformed outcome: %v", err)
	}
}
