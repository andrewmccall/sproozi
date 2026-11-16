package profile

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

// preparingBackend never reports an incarnation from Prepare, as a
// Kubernetes placement whose run waits under a concurrency cap does, and can
// abort such a preparation.
type preparingBackend struct {
	retainedBackend
	pending  bool
	aborted  []session.Launch
	abortErr error
}

func (p *preparingBackend) Prepare(_ context.Context, l session.Launch, _ []byte) (session.Incarnation, error) {
	p.pending = true
	return session.Incarnation{}, errors.New("waiting for the run's Pod")
}

func (p *preparingBackend) Inspect(_ context.Context, l session.Launch) (session.Observation, error) {
	if p.pending {
		return session.Observation{Launch: l, State: session.PlacementPreparing}, nil
	}
	return session.Observation{Launch: l, State: session.PlacementAbsent}, nil
}

func (p *preparingBackend) AbortPreparation(_ context.Context, l session.Launch) error {
	p.aborted = append(p.aborted, l)
	if p.abortErr != nil {
		return p.abortErr
	}
	p.pending = false
	return nil
}

// olderExecutor answers abort-preparation as an executor from before the
// action did: 404 invalid_request, as for any unknown action.
func olderExecutor(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/"+actionAbortPreparation {
			w.Header().Set(headerContentType, jsonMedia)
			writeError(w, http.StatusNotFound, CodeInvalid)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func preparingFixture(t *testing.T, wrap func(http.Handler) http.Handler) (*Client, *preparingBackend, session.Launch, []byte) {
	t.Helper()
	data, digest := artifact(t)
	backend := &preparingBackend{retainedBackend: retainedBackend{data: data, digest: digest}}
	l := launch(digest)
	h, err := NewHandler(Config{Harness: l.Harness, Principal: testPrincipal, Authorizer: tokenAuthorizer(testToken), Placement: l.Placement,
		InstructionDigest: l.InstructionDigest, Allowances: l.Allowances, MaxDuration: time.Hour, MaxCheckpointBytes: testMax, Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	s, ca := serve(t, wrap(h))
	c, err := testClient(s.URL, ca)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, backend, l, data
}

// The action carries the exact launch, needs no incarnation and is not
// refused after the deadline: cancelling is never refused. The executor's
// refusals cross as they do for every action.
func TestAbortPreparationCrossesTheProtocol(t *testing.T) {
	t.Parallel()
	c, b, l, _ := preparingFixture(t, func(h http.Handler) http.Handler { return h })
	l.Deadline = time.Now().Add(-time.Minute).UTC()
	if err := c.AbortPreparation(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	if len(b.aborted) != 1 || !sameLaunch(b.aborted[0], l) {
		t.Fatalf("executor received %+v", b.aborted)
	}
	b.abortErr = &Rejection{Code: CodeConflict}
	remoteCode(t, c.AbortPreparation(t.Context(), l), CodeConflict, http.StatusConflict)
	b.abortErr = errors.New("controller has not ended the run")
	err := c.AbortPreparation(t.Context(), l)
	remoteCode(t, err, CodeUnavailable, http.StatusServiceUnavailable)
	if len(b.aborted) != 3 || strings.Contains(err.Error(), "older") {
		t.Fatalf("an executor's own failure fell back: %d %v", len(b.aborted), err)
	}
}

// The request has the inspect shape: an incarnation or a checkpoint is
// invalid, so an older home's requests are never mistaken for it.
func TestAbortPreparationRefusesOtherPayloads(t *testing.T) {
	t.Parallel()
	c, b, l, data := preparingFixture(t, func(h http.Handler) http.Handler { return h })
	i := session.Incarnation{ExecutionID: l.AllocationID, ResourceUID: "pod"}
	_, err := c.call(t.Context(), actionAbortPreparation, request{Launch: l, Incarnation: &i})
	remoteCode(t, err, CodeInvalid, http.StatusBadRequest)
	_, err = c.call(t.Context(), actionAbortPreparation, request{Launch: l, Checkpoint: data})
	remoteCode(t, err, CodeInvalid, http.StatusBadRequest)
	if len(b.aborted) != 0 {
		t.Fatalf("invalid requests reached the executor: %d", len(b.aborted))
	}
}

// Against an executor from before the action, or one whose placement cannot
// abort a preparation, the client does what a coordinator did before: an
// absent launch is fenced, a prepared one is aborted, and one still preparing
// stays unproven with a message that names the older executor.
func TestAbortPreparationFallsBackOnOlderExecutor(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		fixture func(*testing.T) (*Client, *retainedBackend, session.Launch, []byte)
	}{
		{"older executor", func(t *testing.T) (*Client, *retainedBackend, session.Launch, []byte) {
			c, b, _, l, data := wrappedFixture(t, olderExecutor)
			return c, b, l, data
		}},
		{"placement without abort", func(t *testing.T) (*Client, *retainedBackend, session.Launch, []byte) {
			c, b, _, l, data := fixture(t)
			return c, b, l, data
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			c, b, l, data := test.fixture(t)
			if err := c.AbortPreparation(t.Context(), l); err != nil {
				t.Fatalf("absent launch: %v", err)
			}
			i, err := c.Prepare(t.Context(), l, data)
			if err != nil {
				t.Fatal(err)
			}
			// The fixture's abort reports a stopped writer with no receipt.
			err = c.AbortPreparation(t.Context(), l)
			if !errors.Is(err, errPreparationAbortUnsupported) || !strings.Contains(err.Error(), "usage is not finalized") ||
				b.state.State != session.PlacementStopped || b.state.Incarnation != i {
				t.Fatalf("prepared launch: %v %+v", err, b.state)
			}
			b.state = session.Observation{Launch: l, State: session.PlacementPreparing}
			err = c.AbortPreparation(t.Context(), l)
			if !errors.Is(err, errPreparationAbortUnsupported) || !strings.Contains(err.Error(), "older than this Sproozi") {
				t.Fatalf("preparing launch: %v", err)
			}
		})
	}
}

// On an older executor the fallback aborts only a visit still preparing or
// already stopped. An active visit, or one whose state is unknown (as a
// stopping Kubernetes visit inspects), may have spent, so it is refused and
// left as it is.
func TestAbortPreparationFallbackRefusesVisitThatMayHaveSpent(t *testing.T) {
	t.Parallel()
	for _, state := range []session.PlacementState{session.PlacementActive, session.PlacementUnknown} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			c, b, _, l, data := wrappedFixture(t, olderExecutor)
			i, err := c.Prepare(t.Context(), l, data)
			if err != nil {
				t.Fatal(err)
			}
			b.state.State = state
			err = c.AbortPreparation(t.Context(), l)
			if !errors.Is(err, errPreparationAbortUnsupported) || !strings.Contains(err.Error(), "not a preparation") {
				t.Fatalf("fallback abort of a %s visit: %v", state, err)
			}
			if b.state.State != state || b.state.Incarnation != i {
				t.Fatalf("refused fallback aborted the visit: %+v", b.state)
			}
		})
	}
}

// A home coordinator cancelling a destination whose preparation never
// reported an incarnation reaches cancelled on an executor that supports the
// action, and stays unknown-stop, saying why, on an older one.
func TestCoordinatorCancelsPendingRemoteDestination(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		wrap  func(http.Handler) http.Handler
		phase session.Phase
	}{
		{"current executor", func(h http.Handler) http.Handler { return h }, session.PhaseCancelled},
		{"older executor", olderExecutor, session.PhaseUnknownStop},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			c, b, l, data := preparingFixture(t, test.wrap)
			coordinator, err := session.Open(filepath.Join(t.TempDir(), "sessions"), map[string]session.Placement{l.Placement: c}, testMax)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = coordinator.Close() })
			l.Epoch = 1
			if _, err = coordinator.Create(l, data, ""); err != nil {
				t.Fatal(err)
			}
			if r, err := coordinator.Recover(t.Context(), l.ExecutionID); err == nil || r.Phase != session.PhasePreparing {
				t.Fatalf("pending destination: %s %v", r.Phase, err)
			}
			r, err := coordinator.Cancel(t.Context(), l.ExecutionID, uuid.NewString(), nil)
			if r.Phase != test.phase {
				t.Fatalf("cancel: %s %v", r.Phase, err)
			}
			if test.phase == session.PhaseCancelled && (err != nil || len(b.aborted) != 1) {
				t.Fatalf("supported abort: %v, %d calls", err, len(b.aborted))
			}
			if test.phase == session.PhaseUnknownStop && (!errors.Is(err, errPreparationAbortUnsupported) || len(b.aborted) != 0) {
				t.Fatalf("older executor: %v, %d calls", err, len(b.aborted))
			}
		})
	}
}
