package main

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/deadline"
)

// failAfter bounds a wait that only a failure reaches; nothing waits on it
// to happen.
const failAfter = 30 * time.Second

// holding serves the helper's control socket the way hold does, with grace
// as its reply grace, and returns the socket's path and a channel that
// receives serve's result. It reports through waiting each time the helper
// begins to wait for its client.
func holding(t *testing.T, grace time.Duration) (string, <-chan error, <-chan struct{}) {
	t.Helper()
	// A Unix socket path is limited to about 100 bytes, which a test's own
	// temporary directory can exceed on macOS.
	directory, err := os.MkdirTemp("", "hold")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "control.sock")
	previousSocket, previousGrace, previousAwaiting := helperSocket, finishReplyGrace, awaitingClient
	waiting := make(chan struct{}, 1)
	helperSocket, finishReplyGrace = socket, grace
	awaitingClient = func() { waiting <- struct{}{} }
	t.Cleanup(func() {
		helperSocket, finishReplyGrace, awaitingClient = previousSocket, previousGrace, previousAwaiting
	})
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan error, 1)
	go func() { served <- serve(context.Background(), deadline.At(time.Now().Add(time.Hour)), listener) }()
	t.Cleanup(func() { _ = listener.Close() })
	return socket, served, waiting
}

// askToFinish sends finish on a new connection to socket, as the finish
// command does, without reading the reply.
func askToFinish(t *testing.T, socket string) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(failAfter))
	if _, err = conn.Write([]byte("finish\n")); err != nil {
		t.Fatal(err)
	}
	return conn
}

// The helper is its container's main process, so its exit ends the
// container and kills the finish command it ran, whose exit status the
// placement may then never receive. A client that asked to finish and has
// not yet read the reply keeps the helper running; the reply and the end of
// the stream still reach it, and once it closes the helper exits 0 at once.
// Guards: hold waits for its client after acknowledging a finish, and
// half-closes so the finish command does not wait for it in turn.
func TestFinishedHelperWaitsForItsClientToClose(t *testing.T) {
	socket, served, waiting := holding(t, time.Hour)
	conn := askToFinish(t, socket)
	select {
	case err := <-served:
		t.Fatalf("the helper ended before its client read the reply: %v", err)
	case <-waiting:
	case <-time.After(failAfter):
		t.Fatal("the helper neither waited for its client nor ended")
	}
	reply, err := io.ReadAll(conn)
	if err != nil || string(reply) != "finished\n" {
		t.Fatalf("reply %q: %v", reply, err)
	}
	select {
	case err := <-served:
		t.Fatalf("the helper ended while its client still held the connection: %v", err)
	default:
	}
	_ = conn.Close()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("a finished helper ended with %v, so its container would not exit 0", err)
		}
	case <-time.After(failAfter):
		t.Fatal("the helper did not end once its client closed")
	}
}

// The finish command itself completes against the waiting helper: it reads
// the reply to the end of the stream, which the helper ends by half-closing,
// then closes, and the helper ends. Guards: the half-close, without which
// finish would wait out its own one-second deadline and fail.
func TestFinishCommandCompletesAgainstAWaitingHelper(t *testing.T) {
	privateHelper(t)
	_, served, _ := holding(t, time.Hour)
	if err := finish(); err != nil {
		t.Fatalf("finish: %v", err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(failAfter):
		t.Fatal("the helper did not end once finish closed")
	}
}

// A client that never closes cannot hold the helper, and with it the
// sandbox, past the grace. Guards: the bound on the wait.
func TestFinishedHelperEndsAfterTheGrace(t *testing.T) {
	socket, served, waiting := holding(t, 50*time.Millisecond)
	conn := askToFinish(t, socket)
	reply, err := io.ReadAll(conn)
	if err != nil || string(reply) != "finished\n" {
		t.Fatalf("reply %q: %v", reply, err)
	}
	select {
	case err := <-served:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(failAfter):
		t.Fatal("a client that never closed held the helper open")
	}
	select {
	case <-waiting:
	default:
		t.Fatal("the helper ended without waiting for its client")
	}
}
