//go:build linux || darwin

package home

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
)

func TestUnixControlRejectsUnsafePaths(t *testing.T) {
	address := controlAddress(t)
	if err := os.WriteFile(address, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if listener, err := listenControl(address); err == nil {
		_ = listener.Close()
		t.Fatal("replaced regular control file")
	}
	data, err := os.ReadFile(address)
	if err != nil || string(data) != "keep" {
		t.Fatalf("existing control file changed: %q, %v", data, err)
	}
	if err = os.Remove(address); err != nil {
		t.Fatal(err)
	}
	listener, err := listenControl(address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	if err = os.Chmod(address, 0666); err != nil {
		t.Fatal(err)
	}
	if conn, err := dialControl(context.Background(), address); err == nil {
		_ = conn.Close()
		t.Fatal("connected to shared control socket")
	}
	if other, err := listenControl(address); err == nil {
		_ = other.Close()
		t.Fatal("replaced shared control socket")
	}
	if err = os.Chmod(address, 0600); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(address)
	if err = os.Chmod(parent, 0755); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(parent, 0700) }()
	if conn, err := dialControl(context.Background(), address); err == nil {
		_ = conn.Close()
		t.Fatal("connected through shared control directory")
	}
}

func TestUnixControlReplacesPrivateStaleSocket(t *testing.T) {
	address := controlAddress(t)
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: address, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	if err = os.Chmod(address, 0600); err != nil {
		t.Fatal(err)
	}
	if err = stale.Close(); err != nil {
		t.Fatal(err)
	}
	listener, err := listenControl(address)
	if err != nil {
		t.Fatal(err)
	}
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestUnixControlReportsMissingDaemon(t *testing.T) {
	address := controlAddress(t)
	stale := controlAddress(t)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: stale, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	if err = os.Chmod(stale, 0600); err != nil {
		t.Fatal(err)
	}
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{address, stale, filepath.Join(filepath.Dir(address), "x", "control.sock")} {
		if conn, err := dialControl(context.Background(), path); !errors.Is(err, ErrControlUnavailable) {
			if conn != nil {
				_ = conn.Close()
			}
			t.Fatalf("dialled %s: %v", path, err)
		}
	}
}

func TestControlSocketPathOverPlatformLimitIsNamedBeforeBind(t *testing.T) {
	t.Parallel()
	limit := len(syscall.RawSockaddrUnix{}.Path) - 1
	fits := "/" + strings.Repeat("d", limit-len("/control.sock")-1)
	long := fits + "d"
	want := fmt.Sprintf(`control socket path %q is %d bytes; this platform allows at most %d; `+
		`set a shorter "directory" in config.json`, long+"/control.sock", limit+1, limit)
	for _, problem := range (Config{Directory: fits}).ServeProblems() {
		if strings.HasPrefix(problem, "directory") {
			t.Fatalf("%d-byte control path rejected: %s", limit, problem)
		}
	}
	if !slices.Contains((Config{Directory: long}).ServeProblems(), "directory ("+want+")") {
		t.Fatalf("serve problems lack %q: %q", want, (Config{Directory: long}).ServeProblems())
	}
	// A client reports the same problem instead of "start home".
	if conn, err := dialControl(context.Background(), long+"/control.sock"); err == nil || err.Error() != "home: "+want {
		if conn != nil {
			_ = conn.Close()
		}
		t.Fatalf("client dial: %v, want %q", err, "home: "+want)
	}
}

// A control socket another process still serves is refused, not replaced.
func TestUnixControlRefusesLiveListener(t *testing.T) {
	address := controlAddress(t)
	live, err := net.Listen("unix", address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Close() }()
	if err = os.Chmod(address, 0600); err != nil {
		t.Fatal(err)
	}
	if listener, err := listenControl(address); !errors.Is(err, errControlListening) {
		if listener != nil {
			_ = listener.Close()
		}
		t.Fatalf("listen over a live socket: %v", err)
	}
	conn, err := net.Dial("unix", address)
	if err != nil {
		t.Fatalf("live listener lost its socket: %v", err)
	}
	_ = conn.Close()
	s := &Service{config: Config{Directory: filepath.Dir(address)}, log: io.Discard,
		stacks: []*localStack{{server: &http.Server{Addr: anyLoopbackPort, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13}}}}}
	err = s.Serve(controlContext(t))
	want := "home: control socket " + address + ": another process is listening there; stop it first " +
		"(a home daemon for directory " + filepath.Dir(address) + ", or another program using that path)"
	if err == nil || err.Error() != want {
		t.Fatalf("Serve: %v, want %q", err, want)
	}
}

// A stale socket other accounts can open is named once, with what is unsafe
// and how to clear it.
func TestServeExplainsAnUnsafeStaleSocket(t *testing.T) {
	address := controlAddress(t)
	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: address})
	if err != nil {
		t.Fatal(err)
	}
	stale.SetUnlinkOnClose(false)
	_ = stale.Close()
	if err = os.Chmod(address, 0755); err != nil {
		t.Fatal(err)
	}
	s := &Service{config: Config{Directory: filepath.Dir(address)}, log: io.Discard,
		stacks: []*localStack{{server: &http.Server{Addr: anyLoopbackPort, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13}}}}}
	err = s.Serve(controlContext(t))
	want := "home: control socket " + address + ": the socket allows access by other accounts (mode 0755); " +
		"remove it if no home daemon is running, or run chmod 600 on it, then serve again"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}
