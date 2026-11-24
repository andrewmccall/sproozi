package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// The supervisor serve runs stops its harness at the execution's deadline
// with nothing else asking: the harness is killed, the exit report's cause
// is deadline, and the supervisor exits with an error naming a nonzero code,
// so its process exits 1 (docs/reference/native-control.md).
func TestServeStopsTheHarnessAtItsDeadline(t *testing.T) {
	activatable(t, func() {})
	// A socket path must be short; the test's own directory is too long.
	dir, err := os.MkdirTemp("/tmp", "ns")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	previous := socketPath
	socketPath = filepath.Join(dir, "native", "control.sock")
	t.Cleanup(func() { socketPath = previous })
	s, _ := activation(time.Now().Add(1500 * time.Millisecond))

	var served error
	finished := make(chan struct{})
	out := captureStdout(t, func() {
		go func() {
			served = serve(s.config, false)
			close(finished)
		}()
		var conn net.Conn
		for start := time.Now(); conn == nil; {
			if conn, err = net.Dial("unix", socketPath); err != nil {
				if time.Since(start) > 5*time.Second {
					t.Fatal(err)
				}
				conn = nil
				time.Sleep(10 * time.Millisecond)
			}
		}
		defer func() { _ = conn.Close() }()
		if err := json.NewEncoder(conn).Encode(request{Op: harness.NativeActivate}); err != nil {
			t.Fatal(err)
		}
		var r response
		if err := json.NewDecoder(conn).Decode(&r); err != nil || r.State != harness.NativeActive {
			t.Fatalf("activate: %+v %v", r, err)
		}
		select {
		case <-finished:
		case <-time.After(10 * time.Second):
			t.Error("the supervisor did not stop at its deadline")
		}
	})
	if t.Failed() {
		return
	}
	var reports []harness.NativeExitReport
	for line := range strings.SplitSeq(out, "\n") {
		if r, ok := harness.ParseNativeExitReport([]byte(line)); ok {
			reports = append(reports, r)
		}
	}
	if len(reports) != 1 || reports[0].Cause != harness.NativeExitDeadline || reports[0].Code != -1 {
		t.Fatalf("reports: %+v", reports)
	}
	if served == nil || !strings.Contains(served.Error(), "exit code -1") {
		t.Fatalf("serve returned %v", served)
	}
}
