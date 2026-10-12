package profile

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

type terminalBackend struct {
	*retainedBackend
	input    []byte
	finished chan struct{}
	fail     bool
}

func (b *terminalBackend) Attach(_ context.Context, _ session.Launch, _ session.Incarnation, input io.Reader, output io.Writer) error {
	defer close(b.finished)
	if b.fail {
		return errors.New("private backend failure")
	}
	if _, err := io.WriteString(output, "{\"op\":\"attach\",\"state\":\"active\"}\n"); err != nil {
		return err
	}
	data, err := io.ReadAll(input)
	b.input = data
	if err != nil {
		return err
	}
	_, err = io.WriteString(output, "{\"op\":\"output\",\"data\":\"bmF0aXZlLXR1aQ==\",\"cursor\":10}\n")
	return err
}
func terminalFixture(t *testing.T) (*Client, *terminalBackend, session.Launch, session.Incarnation) {
	t.Helper()
	l := launch(strings.Repeat("c", 64))
	i := session.Incarnation{ExecutionID: uuid.NewString(), ResourceUID: uuid.NewString()}
	b := &terminalBackend{retainedBackend: &retainedBackend{state: session.Observation{Launch: l, Incarnation: i, State: session.PlacementActive}}, finished: make(chan struct{})}
	h, err := NewHandler(Config{Harness: harness.NativeCodex, Principal: testPrincipal, TokenFile: tokenFile(t), Placement: l.Placement, InstructionDigest: l.InstructionDigest, Allowances: l.Allowances, MaxDuration: time.Hour, MaxCheckpointBytes: testMax, Backend: b})
	if err != nil {
		t.Fatal(err)
	}
	s, ca := serve(t, h)
	c, err := NewClient(ClientConfig{URL: s.URL, TokenFile: tokenFile(t), CAFile: ca, MaxCheckpointBytes: testMax})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	return c, b, l, i
}

type signalledOutput struct {
	bytes.Buffer
	once  sync.Once
	ready chan struct{}
}

func (o *signalledOutput) Write(data []byte) (int, error) {
	n, err := o.Buffer.Write(data)
	o.once.Do(func() { close(o.ready) })
	return n, err
}
func TestNativeTerminalFullDuplexAndDisconnect(t *testing.T) {
	c, b, l, i := terminalFixture(t)
	input, write := io.Pipe()
	defer func() { _ = write.Close() }()
	output := &signalledOutput{ready: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- c.Attach(context.Background(), l, i, input, output) }()
	select {
	case <-output.ready:
	case <-time.After(3 * time.Second):
		t.Fatal("server output blocked waiting for terminal input")
	}
	frame := "{\"op\":\"write\",\"data\":\"eA==\"}\n{\"op\":\"resize\",\"rows\":24,\"columns\":80}\n"
	if _, err := io.WriteString(write, frame); err != nil {
		t.Fatal(err)
	}
	_ = write.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("attachment did not close")
	}
	<-b.finished
	if string(b.input) != frame || output.String() != "{\"op\":\"attach\",\"state\":\"active\"}\n{\"op\":\"output\",\"data\":\"bmF0aXZlLXR1aQ==\",\"cursor\":10}\n" {
		t.Fatalf("terminal protocol changed: %q %q", b.input, output.String())
	}
	if b.state.State != session.PlacementActive {
		t.Fatal("disconnect stopped native writer")
	}
}
func TestTerminalCancellationClosesInputAndPreservesWriter(t *testing.T) {
	c, b, l, i := terminalFixture(t)
	input, write := io.Pipe()
	defer func() { _ = write.Close() }()
	output := &signalledOutput{ready: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- c.Attach(ctx, l, i, input, output) }()
	select {
	case <-output.ready:
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("attachment not opened")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("cancel accepted success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client cancellation leaked attachment")
	}
	select {
	case <-b.finished:
	case <-time.After(3 * time.Second):
		t.Fatal("server attachment did not disconnect")
	}
	if b.state.State != session.PlacementActive {
		t.Fatal("cancel stopped native writer")
	}
}
func TestTerminalRejectsControlAndReportsBackendFailure(t *testing.T) {
	c, b, l, i := terminalFixture(t)
	if err := c.Attach(context.Background(), l, i, strings.NewReader("{\"op\":\"configure\",\"config\":{}}\n"), io.Discard); err == nil {
		t.Fatal("control frame accepted")
	}
	<-b.finished
	if len(b.input) != 0 {
		t.Fatal("untrusted control reached native backend")
	}
	c, b, l, i = terminalFixture(t)
	b.fail = true
	err := c.Attach(context.Background(), l, i, strings.NewReader(""), io.Discard)
	if err == nil || strings.Contains(err.Error(), "private") {
		t.Fatalf("backend failure not stable: %v", err)
	}
}
