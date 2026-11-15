//go:build linux || darwin

package nativepty_test

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/nativepty"
	"golang.org/x/sys/unix"
)

// helperRole makes this test binary stand in for a harness process.
const helperRole = "NATIVEPTY_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperRole) {
	case "escape":
		// Leave a descendant in its own session holding the terminal open,
		// then exit 0 by itself. The supervisor waits for that terminal's
		// output to drain before it publishes the exit.
		self, err := os.Executable()
		if err != nil {
			os.Exit(2)
		}
		child := exec.Command(self)
		child.Env = []string{helperRole + "=linger"}
		child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
		child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if child.Start() != nil {
			os.Exit(2)
		}
		os.Exit(0)
	case "linger":
		time.Sleep(time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// A harness that exits by itself keeps that reason even when a stop arrives
// while the supervisor is still confirming its process group and draining
// its output.
func TestNaturalExitIsNotRelabelledByALateStop(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// The window lasts from the leader's reaping until the exit is published,
	// so try a few times for a stop that lands inside it.
	for range 20 {
		s, err := nativepty.Start(nativepty.Launch{Executable: self, Env: []string{helperRole + "=escape"},
			Dir: t.TempDir(), Deadline: time.Now().Add(time.Minute), Size: nativepty.Size{Rows: 24, Columns: 80}})
		if err != nil {
			t.Fatal(err)
		}
		// The leader has been reaped once signal 0 no longer reaches it.
		for unix.Kill(s.PID(), 0) == nil {
			time.Sleep(time.Millisecond)
		}
		raced := true
		select {
		case <-s.Done():
			raced = false
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		exit, err := s.Stop(ctx)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if exit.Reason != nativepty.NativeExit || exit.Code != 0 {
			t.Fatalf("natural exit relabelled: %+v", exit)
		}
		if raced {
			return
		}
	}
	t.Skip("no stop landed between the leader's exit and its publication")
}
