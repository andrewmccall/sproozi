//go:build linux || darwin

package nativepty_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/nativepty"
)

func start(t *testing.T, script string, limit int, deadline time.Time) *nativepty.Supervisor {
	t.Helper()
	s, err := nativepty.Start(nativepty.Launch{Executable: "/bin/sh", Args: []string{"-c", script},
		Env: []string{"PATH=/usr/bin:/bin", "TERM=xterm"}, Dir: t.TempDir(), Deadline: deadline,
		Size: nativepty.Size{Rows: 24, Columns: 80}, ReplayBytes: limit})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := s.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return s
}

func attach(t *testing.T, s *nativepty.Supervisor, cursor uint64) *nativepty.Attachment {
	t.Helper()
	a, err := s.Attach(cursor)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	return a
}

func until(t *testing.T, a *nativepty.Attachment, marker string) nativepty.Output {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var data string
	var result nativepty.Output
	for !strings.Contains(data, marker) {
		out, err := a.Read(ctx)
		if err != nil {
			t.Fatalf("read %q: %v; output=%q", marker, err, data)
		}
		data += string(out.Data)
		result = out
		if out.Exited && !strings.Contains(data, marker) {
			t.Fatalf("process exited before %q: %q", marker, data)
		}
	}
	return result
}

func write(t *testing.T, a *nativepty.Attachment, value string) {
	t.Helper()
	n, err := a.Write([]byte(value))
	if err != nil || n != len(value) {
		t.Fatalf("write: bytes=%d error=%v", n, err)
	}
}

func TestNativeTerminalReconnectGateResizeAndQuit(t *testing.T) {
	script := `stty -echo
if [ -t 0 ] && [ -t 1 ]; then printf 'GENUINE_PTY\n'; else exit 9; fi
while IFS= read -r line; do
  case "$line" in
    /quit) printf 'NATIVE_QUIT\n'; exit 0;;
    size) printf 'SIZE:'; stty size;;
    *) printf 'INPUT:<%s>\n' "$line";;
  esac
done`
	s := start(t, script, 4096, time.Now().Add(10*time.Second))
	a := attach(t, s, 0)
	first := until(t, a, "GENUINE_PTY")
	if _, err := s.Attach(first.Next); !errors.Is(err, nativepty.ErrAttached) {
		t.Fatalf("second input attachment: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
		t.Fatal("detach stopped the native process")
	default:
	}
	b := attach(t, s, 0)
	until(t, b, "GENUINE_PTY")
	if err := b.Resize(nativepty.Size{Rows: 37, Columns: 119}); err != nil {
		t.Fatal(err)
	}
	write(t, b, "size\r")
	until(t, b, "SIZE:37 119")
	s.GateInput()
	if n, err := b.Write([]byte("rejected\r")); n != 0 || !errors.Is(err, nativepty.ErrGated) {
		t.Fatalf("gated write: %d %v", n, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	exit, err := s.QuitAfterCompletedTurn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if exit.Code != 0 || exit.Reason != nativepty.NativeExit {
		t.Fatalf("native quit: %+v", exit)
	}
	last, err := b.Read(ctx)
	if err != nil || !last.Exited || !strings.Contains(string(last.Data), "NATIVE_QUIT") {
		t.Fatalf("final native output: %+v %v", last, err)
	}
}

func TestSlowOutputAttachmentDoesNotBlockProcessAndReportsReplayGap(t *testing.T) {
	script := `stty -echo
i=0
while [ "$i" -lt 5000 ]; do printf '0123456789'; i=$((i+1)); done
printf '\nFINISHED_OUTPUT\n'
while IFS= read -r line; do [ "$line" = /quit ] && exit 0; done`
	s := start(t, script, 128, time.Now().Add(10*time.Second))
	a := attach(t, s, 0)
	// Do not read output. The native process must still reach its input loop.
	write(t, a, "/quit\r")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	exit, err := s.Wait(ctx)
	if err != nil || exit.Code != 0 {
		t.Fatalf("slow reader blocked process: %+v %v", exit, err)
	}
	out, err := a.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Gap || len(out.Data) > 128 || !strings.Contains(string(out.Data), "FINISHED_OUTPUT") {
		t.Fatalf("bounded replay: %+v, text=%q", out, out.Data)
	}
	if out.Next < 50000 {
		t.Fatalf("native output did not finish: cursor=%d", out.Next)
	}
}

func TestDeadlineStopsNativeProcessAndRejectsFurtherInput(t *testing.T) {
	// The process would run for 30 s, so only the deadline can end it first.
	// Waiting for that exit, rather than racing a ready marker against the
	// deadline or bounding the wait, keeps the test independent of load.
	s := start(t, `exec sleep 30`, 256, time.Now().Add(200*time.Millisecond))
	a := attach(t, s, 0)
	exit, err := s.Wait(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if exit.Reason != nativepty.Deadline || exit.Code != -1 {
		t.Fatalf("deadline outcome: %+v", exit)
	}
	if n, err := a.Write([]byte("after\r")); n != 0 || !errors.Is(err, nativepty.ErrExited) {
		t.Fatalf("post-deadline input: %d %v", n, err)
	}
}

func TestDetachedReaderWakesAndNewReaderReceivesOutput(t *testing.T) {
	s := start(t, `stty -echo; printf 'READY\n'; while IFS= read -r line; do printf '<%s>\n' "$line"; done`, 256, time.Now().Add(10*time.Second))
	a := attach(t, s, 0)
	first := until(t, a, "READY")
	result := make(chan error, 1)
	go func() { _, err := a.Read(context.Background()); result <- err }()
	_ = a.Close()
	select {
	case err := <-result:
		if !errors.Is(err, nativepty.ErrDetached) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("detach did not wake output reader")
	}
	b := attach(t, s, first.Next)
	write(t, b, "fresh\r")
	until(t, b, "<fresh>")
}

func TestNativeQuitRequiresInputGate(t *testing.T) {
	s := start(t, `stty -echo; printf 'READY\n'; while IFS= read -r line; do [ "$line" = /quit ] && exit 0; done`, 256, time.Now().Add(10*time.Second))
	a := attach(t, s, 0)
	until(t, a, "READY")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := s.QuitAfterCompletedTurn(ctx); err == nil {
		t.Fatal("ungated native quit succeeded")
	}
	select {
	case <-s.Done():
		t.Fatal("rejected native quit stopped process")
	default:
	}
	s.GateInput()
	exit, err := s.QuitAfterCompletedTurn(ctx)
	if err != nil || exit.Code != 0 {
		t.Fatalf("gated native quit: %+v %v", exit, err)
	}
}
