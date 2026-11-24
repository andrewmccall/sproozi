package main

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/deadline"
)

// failAfter bounds a wait that only a failure reaches; nothing waits on it
// to happen.
const failAfter = 30 * time.Second

// watchedConn is the helper's side of one connection. It reports when the
// helper reads again after it replied, which is its wait for the client to
// close, and the read deadline it set for that wait.
type watchedConn struct {
	*net.UnixConn
	replied atomic.Bool
	// waiting is closed when the helper reads after its reply.
	waiting chan struct{}
	once    sync.Once
	mu      sync.Mutex
	until   time.Time
}

func (c *watchedConn) Write(b []byte) (int, error) {
	n, err := c.UnixConn.Write(b)
	c.replied.Store(true)
	return n, err
}

func (c *watchedConn) Read(b []byte) (int, error) {
	if c.replied.Load() {
		c.once.Do(func() { close(c.waiting) })
	}
	return c.UnixConn.Read(b)
}

func (c *watchedConn) SetReadDeadline(t time.Time) error {
	if c.replied.Load() {
		c.mu.Lock()
		c.until = t
		c.mu.Unlock()
	}
	return c.UnixConn.SetReadDeadline(t)
}

func (c *watchedConn) waitDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.until
}

// watchedListener hands each connection it accepts to the test as well.
type watchedListener struct {
	net.Listener
	accepted chan *watchedConn
}

func (l watchedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	watched := &watchedConn{UnixConn: conn.(*net.UnixConn), waiting: make(chan struct{})}
	l.accepted <- watched
	return watched, nil
}

// socketPath is a fresh control socket path. A Unix socket path is limited
// to about 100 bytes, which a test's own temporary directory can exceed on
// macOS.
func socketPath(t *testing.T) string {
	t.Helper()
	directory, err := os.MkdirTemp("", "hold")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	socket := filepath.Join(directory, "control.sock")
	previous := helperSocket
	helperSocket = socket
	t.Cleanup(func() { helperSocket = previous })
	return socket
}

// holding serves the helper's control socket the way hold does, with grace
// as its reply grace. It returns the socket's path, a channel that receives
// serve's result and one that receives each connection the helper accepts.
func holding(t *testing.T, grace time.Duration) (string, <-chan error, <-chan *watchedConn) {
	t.Helper()
	socket := socketPath(t)
	previous := finishReplyGrace
	finishReplyGrace = grace
	t.Cleanup(func() { finishReplyGrace = previous })
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan *watchedConn, 4)
	served := make(chan error, 1)
	go func() {
		served <- serve(context.Background(), deadline.At(time.Now().Add(time.Hour)), watchedListener{listener, accepted})
	}()
	return socket, served, accepted
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
// placement may then never receive. After it replies, the helper reads
// until its client closes, with a read deadline of the grace, so a client
// that has not yet read the reply keeps it running; the reply and the end
// of the stream still reach that client, and once it closes the helper
// exits 0 at once. Guards: the wait (io.Copy after the reply), its read
// deadline, and the half-close the client reads to.
func TestFinishedHelperWaitsForItsClientToClose(t *testing.T) {
	socket, served, accepted := holding(t, time.Hour)
	before := time.Now()
	client := askToFinish(t, socket)
	conn := <-accepted
	select {
	case err := <-served:
		t.Fatalf("the helper ended before its client read the reply: %v", err)
	case <-conn.waiting:
	case <-time.After(failAfter):
		t.Fatal("the helper neither waited for its client nor ended")
	}
	// The wait's deadline is the grace from the reply, set before the wait.
	if until := conn.waitDeadline(); until.Before(before.Add(time.Hour)) || until.After(time.Now().Add(time.Hour)) {
		t.Fatalf("the helper waits for its client until %v, not for the grace", until)
	}
	reply, err := io.ReadAll(client)
	if err != nil || string(reply) != "finished\n" {
		t.Fatalf("reply %q: %v", reply, err)
	}
	// The helper is blocked reading until its client closes or an hour
	// passes, so it cannot have ended.
	select {
	case err := <-served:
		t.Fatalf("the helper ended while its client still held the connection: %v", err)
	default:
	}
	_ = client.Close()
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

// The finish command keeps its side open for writing until it has the
// reply: a half-close after the request would end the helper's wait before
// the command read anything, which is the race the wait closes. A server
// that answers only once the request side ends therefore never answers it,
// and finish fails at its own deadline. Guards: that finish does not
// half-close.
func TestFinishCommandKeepsItsRequestOpenUntilTheReply(t *testing.T) {
	privateHelper(t)
	listener, err := net.Listen("unix", socketPath(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		request := make([]byte, len(finishRequest))
		if _, err = io.ReadFull(conn, request); err != nil {
			return
		}
		// Only the end of the request side, a half-close or a close, ends
		// this read; then reply.
		_, _ = io.Copy(io.Discard, conn)
		_, _ = conn.Write([]byte(finishReply))
	}()
	if err := finish(); err == nil {
		t.Fatal("finish ended its request side before it read the reply")
	}
}

// A client that never closes cannot hold the helper, and with it the
// sandbox, past the grace. Guards: the bound on the wait.
func TestFinishedHelperEndsAfterTheGrace(t *testing.T) {
	socket, served, accepted := holding(t, 50*time.Millisecond)
	client := askToFinish(t, socket)
	reply, err := io.ReadAll(client)
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
	case <-(<-accepted).waiting:
	default:
		t.Fatal("the helper ended without waiting for its client")
	}
}

// The grace stays short: it delays the sandbox's end only for a client that
// hangs, whose own deadline is one second, and the placement's wait for the
// helper's end after a lost finish (helperEndWait, 15 seconds, in
// internal/runtime/kubernetes/placement) must outlast it. Guards: the
// production grace.
func TestFinishReplyGraceIsTenSeconds(t *testing.T) {
	if finishReplyGrace != 10*time.Second {
		t.Fatalf("finishReplyGrace is %s; the placement waits 15 seconds for the helper's end", finishReplyGrace)
	}
}
