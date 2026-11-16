package profile

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session"
)

// exitingBackend is a placement that observes its active visit's exit.
type exitingBackend struct {
	retainedBackend
	exited bool
}

func (p *exitingBackend) Exited(_ context.Context, l session.Launch, i session.Incarnation) (bool, error) {
	if !sameLaunch(l, p.state.Launch) || i != p.state.Incarnation {
		return false, &Rejection{Code: CodeConflict}
	}
	return p.exited && p.state.State == session.PlacementActive, nil
}

// activeExitFixture starts an active visit on backend through wrap.
func activeExitFixture(t *testing.T, backend session.Placement, wrap func(http.Handler) http.Handler) (*Client, session.Launch, session.Incarnation) {
	t.Helper()
	c, l := backendFixture(t, backend, wrap)
	data, _ := artifact(t)
	i, err := c.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	return c, l, i
}

func exitingFixture(t *testing.T, wrap func(http.Handler) http.Handler) (*Client, *exitingBackend, session.Launch, session.Incarnation) {
	t.Helper()
	data, digest := artifact(t)
	backend := &exitingBackend{retainedBackend: retainedBackend{data: data, digest: digest}}
	c, l, i := activeExitFixture(t, backend, wrap)
	return c, backend, l, i
}

// The executor reports an active visit's exit to home, so home's exit watcher
// can stop it; a stopped visit, or another incarnation, reports none.
func TestExitedCrossesTheProtocol(t *testing.T) {
	t.Parallel()
	c, b, l, i := exitingFixture(t, func(h http.Handler) http.Handler { return h })
	if exited, err := c.Exited(t.Context(), l, i); err != nil || exited {
		t.Fatalf("a running harness reported as exited: %v %v", exited, err)
	}
	b.exited = true
	if exited, err := c.Exited(t.Context(), l, i); err != nil || !exited {
		t.Fatalf("an exited harness not reported: %v %v", exited, err)
	}
	other := i
	other.ResourceUID = "another-pod"
	_, err := c.Exited(t.Context(), l, other)
	remoteCode(t, err, CodeConflict, http.StatusConflict)
	if _, err = c.Stop(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	if exited, err := c.Exited(t.Context(), l, i); err != nil || exited {
		t.Fatalf("a stopped visit reported as exited: %v %v", exited, err)
	}
}

// olderExitExecutor answers exited as an executor from before the action
// does, and counts the requests it refused.
func olderExitExecutor(refused *atomic.Int32) func(http.Handler) http.Handler {
	return func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/"+actionExited {
				refused.Add(1)
				w.Header().Set(headerContentType, jsonMedia)
				writeError(w, http.StatusNotFound, CodeInvalid)
				return
			}
			h.ServeHTTP(w, r)
		})
	}
}

// Against an executor older than the action, home sees what it saw before
// the action existed: no exit and no error, and the connection asks once.
func TestExitedAgainstAnOlderExecutorReportsNone(t *testing.T) {
	t.Parallel()
	var refused atomic.Int32
	c, b, l, i := exitingFixture(t, olderExitExecutor(&refused))
	b.exited = true
	for range 3 {
		if exited, err := c.Exited(t.Context(), l, i); err != nil || exited {
			t.Fatalf("an older executor: %v %v", exited, err)
		}
	}
	if refused.Load() != 1 {
		t.Fatalf("the connection asked an older executor %d times", refused.Load())
	}
	// The visit is untouched: the ordinary stop still works.
	if _, err := c.Stop(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
}

// An executor whose placement cannot observe exits answers unsupported, which
// home reads as no exit.
func TestExitedWithoutAnObservingPlacementReportsNone(t *testing.T) {
	t.Parallel()
	data, digest := artifact(t)
	c, l, i := activeExitFixture(t, &retainedBackend{data: data, digest: digest}, func(h http.Handler) http.Handler { return h })
	if exited, err := c.Exited(t.Context(), l, i); err != nil || exited {
		t.Fatalf("a placement that cannot observe exits: %v %v", exited, err)
	}
}

// exited is authorized and validated as every other placement action: only
// the home principal may ask, and only about an exact incarnation. Answers to
// the other actions never carry the field, so an older home reads them as it
// always did.
func TestExitedIsAuthorizedAndValidatedAsTheOtherActions(t *testing.T) {
	t.Parallel()
	_, b, s, l, _ := fixture(t)
	i := session.Incarnation{ExecutionID: l.AllocationID, ResourceUID: "pod"}
	b.state = session.Observation{Launch: l, Incarnation: i, State: session.PlacementActive}
	withIncarnation, err := json.Marshal(request{Version: protocolVersion, Launch: l, Incarnation: &i})
	if err != nil {
		t.Fatal(err)
	}
	if status, _, _ := raw(t, s, "/v1/"+actionExited, withIncarnation, map[string]string{headerAuthorization: bearerPrefix + strings.Repeat("x", 40)}); status != http.StatusUnauthorized {
		t.Fatalf("an unknown token asked about an exit: %d", status)
	}
	without, err := json.Marshal(request{Version: protocolVersion, Launch: l})
	if err != nil {
		t.Fatal(err)
	}
	if status, _, _ := raw(t, s, "/v1/"+actionExited, without, nil); status != http.StatusBadRequest {
		t.Fatalf("exited without an incarnation: %d", status)
	}
	// The fixture's placement observes no exits.
	if status, body, _ := raw(t, s, "/v1/"+actionExited, withIncarnation, nil); status != http.StatusUnprocessableEntity || strings.Contains(string(body), `"exited"`) {
		t.Fatalf("exited on a placement that cannot observe exits: %d %s", status, body)
	}
	inspect, err := json.Marshal(request{Version: protocolVersion, Launch: l})
	if err != nil {
		t.Fatal(err)
	}
	if status, body, _ := raw(t, s, "/v1/"+actionInspect, inspect, nil); status != http.StatusOK || strings.Contains(string(body), `"exited"`) {
		t.Fatalf("inspect answer: %d %s", status, body)
	}
}
