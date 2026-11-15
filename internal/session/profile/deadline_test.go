package profile

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

// noDeadlineClient serves an executor with the given maxDuration (0: none).
func noDeadlineClient(t *testing.T, limit time.Duration, backend session.Placement, l session.Launch) *Client {
	t.Helper()
	h, err := NewHandler(Config{Harness: l.Harness, Principal: testPrincipal, Authorizer: tokenAuthorizer(testToken),
		Placement: l.Placement, InstructionDigest: l.InstructionDigest, Allowances: l.Allowances, MaxDuration: limit,
		MaxCheckpointBytes: testMax, Backend: backend})
	if err != nil {
		t.Fatal(err)
	}
	s, ca := serve(t, h)
	c, err := testClient(s.URL, ca)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c
}

// An executor with a maxDuration denies a launch with no deadline (ADR 0056).
func TestCappedExecutorDeniesNoDeadline(t *testing.T) {
	t.Parallel()
	data, digest := artifact(t)
	l := launch(digest)
	l.Deadline = time.Time{}
	c := noDeadlineClient(t, time.Hour, &retainedBackend{data: data, digest: digest}, l)
	_, err := c.Prepare(context.Background(), l, data)
	remoteCode(t, err, CodeDenied, 403)
}

// An executor without a maxDuration admits a launch with no deadline, which
// then prepares, activates, stops and seals like any other.
func TestUncappedExecutorRunsWithoutDeadline(t *testing.T) {
	t.Parallel()
	data, digest := artifact(t)
	l := launch(digest)
	l.Deadline = time.Time{}
	c := noDeadlineClient(t, 0, &retainedBackend{data: data, digest: digest}, l)
	ctx := context.Background()
	i, err := c.Prepare(ctx, l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Activate(ctx, l, i); err != nil {
		t.Fatal(err)
	}
	s, err := c.Stop(ctx, l, i)
	if err != nil || !s.ActualStopped || !bytes.Equal(s.Checkpoint, data) || !s.Launch.Deadline.IsZero() {
		t.Fatalf("stop without deadline: %+v %v", s, err)
	}
}

// An attachment to an execution without a deadline is not cut off at once.
func TestAttachWithoutDeadline(t *testing.T) {
	l := launch(strings.Repeat("c", 64))
	l.Deadline = time.Time{}
	i := session.Incarnation{ExecutionID: uuid.NewString(), ResourceUID: uuid.NewString()}
	b := &terminalBackend{retainedBackend: &retainedBackend{state: session.Observation{Launch: l, Incarnation: i,
		State: session.PlacementActive}}, finished: make(chan struct{})}
	c := noDeadlineClient(t, 0, b, l)
	output := &signalledOutput{ready: make(chan struct{})}
	if err := c.Attach(context.Background(), l, i, io.NopCloser(strings.NewReader("")), output); err != nil {
		t.Fatal(err)
	}
	<-b.finished
	if !strings.Contains(output.String(), `"op":"output"`) {
		t.Fatalf("attachment without deadline ended early: %q", output.String())
	}
}
