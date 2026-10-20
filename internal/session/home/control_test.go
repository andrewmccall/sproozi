package home

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/session"
)

// anyLoopbackPort asks the system for a free loopback port.
const anyLoopbackPort = "127.0.0.1:0"

const controlTestID = "03131e83-2aba-44ef-90b0-61668e24552a"

func controlAddress(t *testing.T) string {
	t.Helper()
	dir, err := privatefs.TempDir("", "sc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return SocketPath(dir)
}

func controlContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// A client that finds no home daemon reports it the same way on every platform.
func TestControlReportsMissingDaemon(t *testing.T) {
	if conn, err := dialControl(controlContext(t), controlAddress(t)); !errors.Is(err, ErrControlUnavailable) {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatalf("dialled missing daemon: %v", err)
	}
}

func TestLocalControlRequestAndRestart(t *testing.T) {
	address := controlAddress(t)
	for range 2 {
		listener, err := listenControl(address)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				done <- err
				return
			}
			defer func() { _ = conn.Close() }()
			var request Request
			if err = json.NewDecoder(conn).Decode(&request); err == nil {
				if request.Action != ActionStatus || request.ID != controlTestID {
					err = fmt.Errorf("unexpected request: %+v", request)
				} else {
					err = json.NewEncoder(conn).Encode(Response{Status: &Status{ID: controlTestID, Phase: "active"}})
				}
			}
			done <- err
		}()
		response, callErr := Call(controlContext(t), address, Request{Action: ActionStatus, ID: controlTestID})
		closeErr := listener.Close()
		if err = <-done; err != nil {
			t.Fatal(err)
		}
		if err = errors.Join(callErr, closeErr); err != nil {
			t.Fatal(err)
		}
		if response.Status == nil || response.Status.ID != controlTestID || response.Status.Phase != "active" {
			t.Fatalf("unexpected response: %+v", response)
		}
	}
}

func TestLocalControlCancellation(t *testing.T) {
	listener, err := listenControl(controlAddress(t))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	ctx, cancel := context.WithCancel(controlContext(t))
	defer cancel()
	accepted := make(chan net.Conn, 1)
	server := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			server <- acceptErr
			return
		}
		var request Request
		if decodeErr := json.NewDecoder(conn).Decode(&request); decodeErr != nil {
			_ = conn.Close()
			server <- decodeErr
			return
		}
		accepted <- conn
	}()
	result := make(chan error, 1)
	go func() {
		_, callErr := Call(ctx, listener.Addr().String(), Request{Action: ActionStatus, ID: controlTestID})
		result <- callErr
	}()
	select {
	case conn := <-accepted:
		defer func() { _ = conn.Close() }()
	case err = <-server:
		t.Fatalf("accept request: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	cancel()
	select {
	case err = <-result:
		if err == nil {
			t.Fatal("canceled call succeeded")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("canceled call retained its connection")
	}
}

func TestLocalControlTerminalFrames(t *testing.T) {
	address := controlAddress(t)
	listener, err := listenControl(address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	resized := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = conn.Close() }()
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		decoder, encoder := json.NewDecoder(conn), json.NewEncoder(conn)
		var request Request
		if err = decoder.Decode(&request); err != nil {
			done <- err
			return
		}
		if request.Action != ActionAttach || request.ID != controlTestID {
			done <- fmt.Errorf("unexpected attach: %+v", request)
			return
		}
		if err = encoder.Encode(Response{}); err == nil {
			err = encoder.Encode(terminalFrame{Data: []byte("prompt")})
		}
		var frame terminalFrame
		if err == nil {
			err = decoder.Decode(&frame)
		}
		if err != nil {
			done <- err
			return
		}
		if frame.Op != terminalResize || frame.Rows != 32 || frame.Columns != 100 {
			done <- fmt.Errorf("unexpected resize: %+v", frame)
			return
		}
		close(resized)
		if err = decoder.Decode(&frame); err != nil {
			done <- err
			return
		}
		if frame.Op != terminalWrite || string(frame.Data) != "hello" {
			done <- fmt.Errorf("unexpected input: %+v", frame)
			return
		}
		done <- encoder.Encode(terminalFrame{Op: terminalWrite, Written: 5})
	}()
	input, writer := io.Pipe()
	defer func() { _ = input.Close(); _ = writer.Close() }()
	go func() {
		select {
		case <-resized:
			_, _ = writer.Write([]byte("hello\x1d"))
		case <-time.After(5 * time.Second):
		}
		_ = writer.Close()
	}()
	sizes := make(chan TerminalSize, 1)
	sizes <- TerminalSize{Rows: 32, Columns: 100}
	var output bytes.Buffer
	err = AttachTerminal(controlContext(t), address, controlTestID, input, &output, sizes)
	if err != nil {
		t.Fatal(err)
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if output.String() != "prompt" {
		t.Fatalf("output = %q", output.String())
	}
}

func TestLocalControlRejectsMalformedFrames(t *testing.T) {
	for _, frame := range []string{"{bad json}\n", "{\"action\":\"unrecognized\"}\n", strings.Repeat("x", controlLimit+1) + "\n"} {
		address := controlAddress(t)
		listener, err := listenControl(address)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			conn, err := listener.Accept()
			if err != nil {
				done <- err
				return
			}
			defer func() { _ = conn.Close() }()
			new(Service).handle(context.Background(), conn)
			done <- nil
		}()
		conn, err := dialControl(controlContext(t), address)
		if err != nil {
			_ = listener.Close()
			t.Fatal(err)
		}
		_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
		_, _ = io.WriteString(conn, frame)
		line, readErr := readLine(bufio.NewReader(conn), responseLimit)
		_ = conn.Close()
		_ = listener.Close()
		if err = <-done; err != nil {
			t.Fatal(err)
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		var response Response
		if err = json.Unmarshal(line, &response); err != nil {
			t.Fatal(err)
		}
		if response.Error != "home: invalid control request" {
			t.Fatalf("malformed frame response: %+v", response)
		}
	}
}

// lines collects writes so a test can wait for one without racing the writer.
type lines chan string

func (l lines) Write(p []byte) (int, error) { l <- string(p); return len(p), nil }

// Serve names its control socket and gateway address once both listen.
func TestServeLogsItsListeners(t *testing.T) {
	dir, err := privatefs.TempDir("", "sl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	log := make(lines, 1)
	s := &Service{config: Config{Directory: dir}, log: log,
		stacks: []*localStack{{server: &http.Server{Addr: anyLoopbackPort, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13}}}}}
	ctx, cancel := context.WithCancel(controlContext(t))
	served := make(chan error, 1)
	go func() { served <- s.Serve(ctx) }()
	select {
	case line := <-log:
		prefix := "Started home daemon: control socket " + SocketPath(dir) + ", gateway 127.0.0.1:"
		if !strings.HasPrefix(line, prefix) || !strings.HasSuffix(line, "\n") || strings.Count(line, "\n") != 1 {
			t.Errorf("start line %q, want prefix %q", line, prefix)
		}
	case err = <-served:
		t.Fatalf("serve stopped before logging: %v", err)
	}
	cancel()
	if line := <-log; line != "Stopped home daemon\n" {
		t.Errorf("stop line %q", line)
	}
	if err = <-served; err != nil {
		t.Fatal(err)
	}
}

// A gateway address another process holds names gateway.listen and the fix.
func TestServeNamesTheGatewayAddressInUse(t *testing.T) {
	dir, err := privatefs.TempDir("", "sb-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	held, err := net.Listen("tcp4", anyLoopbackPort)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	address := held.Addr().String()
	s := &Service{config: Config{Directory: dir}, log: io.Discard,
		stacks: []*localStack{{server: &http.Server{Addr: address, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13}}}}}
	err = s.Serve(controlContext(t))
	want := "home: gateway.listen \"" + address + "\": address already in use; stop the other process or choose " +
		"another port (and local.gatewayPort)"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

// A move or stop blocked by an unfinished earlier operation names the commands
// that unblock it and is a rejection, so the CLI offers no looping retry.
func TestRecoveryRequiredNamesRecoverAndCancel(t *testing.T) {
	id := "6f0f4f3e-0f6b-4bd8-9a39-3f1c1f2b8b8e"
	err := needsRecovery(fmt.Errorf("session: previous move requires recovery: %w", session.ErrRecoveryRequired), id)
	want := "session: previous move requires recovery: session: recovery required; run \"sproozi recover " + id +
		"\" to finish it, or \"sproozi cancel " + id + "\" to revoke and force-stop its current execution"
	if !errors.Is(err, ErrRejected) || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	if err = needsRecovery(errors.New("other"), id); errors.Is(err, ErrRejected) {
		t.Fatal("unrelated error became a rejection")
	}
}

// The serve preflight names a gateway address in use without touching the
// home directory.
func TestCheckListenNamesTheGatewayAddressInUse(t *testing.T) {
	held, err := net.Listen("tcp4", anyLoopbackPort)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	address := held.Addr().String()
	missing := t.TempDir() + "/home"
	err = CheckListen(controlContext(t), Config{Directory: missing, Gateway: GatewayConfig{Listen: address}})
	want := "home: gateway.listen \"" + address + "\": address already in use; stop the other process or choose " +
		"another port (and local.gatewayPort)"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	if _, statErr := os.Stat(missing); !os.IsNotExist(statErr) {
		t.Fatalf("preflight created the home directory: %v", statErr)
	}
	if err = CheckListen(controlContext(t), Config{Gateway: GatewayConfig{Listen: anyLoopbackPort}}); err != nil {
		t.Fatal(err)
	}
}

// A second serve for a directory whose home daemon answers its control socket
// names that daemon, not the gateway port it holds.
func TestCheckListenNamesTheRunningHomeDaemon(t *testing.T) {
	dir, err := privatefs.TempDir("", "sd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	held, err := net.Listen("tcp4", anyLoopbackPort)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = held.Close() }()
	control, err := listenControl(socketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = control.Close() }()
	go func() {
		for {
			conn, acceptErr := control.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	err = CheckListen(controlContext(t), Config{Directory: dir, Gateway: GatewayConfig{Listen: held.Addr().String()}})
	want := "home: another home daemon is using directory " + dir +
		"; stop it first (\"sproozi serve\" holds it until it exits)"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

// A failed native terminal frame names the reason and the next step, and shows
// only printable text from inside the sandbox.
func TestTerminalFailureNamesReasonAndNextStep(t *testing.T) {
	id := controlTestID
	err := terminalFailure(id, terminalFrame{Op: "attach", Error: "native execution is not active"})
	want := "home: session " + id + " has no running native process to attach to; run \"sproozi status " + id +
		"\" to see its phase: \"sproozi recover " + id + "\" finishes an unfinished start or move, and " +
		"\"sproozi resume " + id + "\" continues a stopped session"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
	err = terminalFailure(id, terminalFrame{Op: terminalResize, Error: "bad\x1b[2Jsize"})
	want = "home: native terminal resize failed: bad[2Jsize; run \"sproozi status " + id + "\" to see the session's phase"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}
