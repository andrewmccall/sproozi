package profile

import (
	"context"
	"crypto/x509"
	"errors"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

// heldBackend is a placement whose stop lasts as long as a test says, and
// which runs one action at a time, as the Kubernetes placement's operation
// lock does.
type heldBackend struct {
	*retainedBackend
	lock sync.Mutex
	// stopTakes is how long each stop lasts.
	stopTakes time.Duration
	// entered, when set, is closed as the first stop starts; that stop then
	// lasts until release is closed or its context ends.
	entered chan struct{}
	release chan struct{}

	mu sync.Mutex
	// ends is what each stop's context said when the stop ended.
	ends []error
}

func (b *heldBackend) Stop(ctx context.Context, l session.Launch, i session.Incarnation) (session.Stopped, error) {
	b.lock.Lock()
	defer b.lock.Unlock()
	end := func(err error) error {
		b.mu.Lock()
		b.ends = append(b.ends, err)
		b.mu.Unlock()
		return err
	}
	if b.entered != nil {
		close(b.entered)
		b.entered = nil
		select {
		case <-b.release:
		case <-ctx.Done():
			return session.Stopped{}, end(ctx.Err())
		}
	}
	select {
	case <-time.After(b.stopTakes):
	case <-ctx.Done():
		return session.Stopped{}, end(ctx.Err())
	}
	_ = end(ctx.Err())
	return b.retainedBackend.Stop(ctx, l, i)
}

func (b *heldBackend) Abort(ctx context.Context, l session.Launch, i session.Incarnation) (session.Observation, error) {
	b.lock.Lock()
	defer b.lock.Unlock()
	return b.retainedBackend.Abort(ctx, l, i)
}

func (b *heldBackend) stopEnds() []error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]error(nil), b.ends...)
}

// heldFixture serves a heldBackend whose visit is active to a client whose
// short requests time out after timeout. ended receives once for each stop
// request whose context the executor saw end.
func heldFixture(t *testing.T, timeout time.Duration) (*Client, *heldBackend, *handler, session.Launch, session.Incarnation, <-chan struct{}) {
	t.Helper()
	data, digest := artifact(t)
	b := &heldBackend{retainedBackend: &retainedBackend{data: data, digest: digest}}
	l := launch(digest)
	i := session.Incarnation{ExecutionID: l.AllocationID, ResourceUID: uuid.NewString()}
	b.state = session.Observation{Launch: l, Incarnation: i, State: session.PlacementActive}
	served, err := NewHandler(Config{Harness: l.Harness, Principal: testPrincipal, Authorizer: tokenAuthorizer(testToken),
		Placement: l.Placement, InstructionDigest: l.InstructionDigest, Allowances: l.Allowances, MaxDuration: time.Hour,
		MaxCheckpointBytes: testMax, Backend: b})
	if err != nil {
		t.Fatal(err)
	}
	ended := make(chan struct{}, 16)
	s, ca := serve(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/"+actionStop {
			go func() {
				<-r.Context().Done()
				ended <- struct{}{}
			}()
		}
		served.ServeHTTP(w, r)
	}))
	pem, err := os.ReadFile(ca)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem)
	c, err := Connect(Connection{Origin: s.URL, Token: testToken, Roots: roots, MaxCheckpointBytes: testMax, Timeout: timeout})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	h := served.(*handler)
	// A failing test must not keep the server's cleanup waiting for a held
	// stop for the whole of StopBound.
	h.stopBound = 10 * time.Second
	return c, b, h, l, i, ended
}

func awaitSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not happen", what)
	}
}

// A stop takes as long as its turn, its quit, its container and its capture
// need, which can be minutes. The client's short request timeout (here 50
// milliseconds, standing for 45 seconds) does not cut it off: the stop's own
// wait applies, and home gets the executor's answer, not a transport error.
func TestStopLongerThanTheShortTimeoutIsAnswered(t *testing.T) {
	t.Parallel()
	c, b, _, l, i, _ := heldFixture(t, 50*time.Millisecond)
	b.stopTakes = 300 * time.Millisecond
	s, err := c.Stop(t.Context(), l, i)
	if err != nil || s.Receipt == nil || len(s.Checkpoint) == 0 {
		t.Fatalf("long stop: %+v %v", s, err)
	}
}

// The turn-pending answer, which the executor gives about two minutes after
// it asked, arrives well inside the stop's wait too.
func TestTurnPendingAfterTheShortTimeoutIsAnswered(t *testing.T) {
	t.Parallel()
	c, b, _, l, i, _ := heldFixture(t, 50*time.Millisecond)
	b.stopTakes = 300 * time.Millisecond
	b.waiting = testWaiting
	_, err := c.Stop(t.Context(), l, i)
	if !errors.Is(err, harness.ErrNativeTurnPending) {
		t.Fatalf("long turn-pending stop: %v", err)
	}
	remoteCode(t, err, CodeTurnPending, http.StatusServiceUnavailable)
}

// A stop whose request ends, because home went away or gave up, goes on on
// the executor to its outcome; the retry of the same stop waits for it and
// picks it up.
func TestStopOutlivesItsRequest(t *testing.T) {
	t.Parallel()
	c, b, _, l, i, ended := heldFixture(t, 0)
	b.entered, b.release = make(chan struct{}), make(chan struct{})
	entered := b.entered
	ctx, leave := context.WithCancel(t.Context())
	first := make(chan error, 1)
	go func() {
		_, err := c.Stop(ctx, l, i)
		first <- err
	}()
	awaitSignal(t, entered, "the stop starting")
	leave()
	if err := <-first; err == nil {
		t.Fatal("a stop home left was answered")
	}
	awaitSignal(t, ended, "the executor noticing that home left")
	close(b.release)
	s, err := c.Stop(t.Context(), l, i)
	if err != nil || s.Receipt == nil || len(s.Checkpoint) == 0 {
		t.Fatalf("retried stop: %+v %v", s, err)
	}
	if ends := b.stopEnds(); len(ends) != 2 || ends[0] != nil || ends[1] != nil {
		t.Fatalf("the executor gave up the stop home left: %v", ends)
	}
}

// Cancellation still never waits for a turn: an abort for the visit
// interrupts a stop the executor carries on after its request ended.
func TestAbortInterruptsAStopWhoseRequestEnded(t *testing.T) {
	t.Parallel()
	c, b, _, l, i, ended := heldFixture(t, 0)
	b.entered, b.release = make(chan struct{}), make(chan struct{})
	entered := b.entered
	ctx, leave := context.WithCancel(t.Context())
	first := make(chan error, 1)
	go func() {
		_, err := c.Stop(ctx, l, i)
		first <- err
	}()
	awaitSignal(t, entered, "the stop starting")
	leave()
	<-first
	awaitSignal(t, ended, "the executor noticing that home left")
	aborted := make(chan error, 1)
	go func() {
		o, err := c.Abort(t.Context(), l, i)
		if err == nil && o.State != session.PlacementStopped {
			err = errors.New("abort did not stop the visit")
		}
		aborted <- err
	}()
	select {
	case err := <-aborted:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("abort waited for the stop's turn")
	}
	if ends := b.stopEnds(); len(ends) != 1 || !errors.Is(ends[0], context.Canceled) {
		t.Fatalf("the interrupted stop ended with %v", ends)
	}
}
