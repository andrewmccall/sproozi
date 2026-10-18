package home

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/privatefs"
)

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
		if frame.Op != "resize" || frame.Rows != 32 || frame.Columns != 100 {
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
