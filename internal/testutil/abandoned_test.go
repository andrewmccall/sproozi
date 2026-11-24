package testutil

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// RemoveAbandoned removes the directory of a process that has exited and
// keeps those of this process, a live process and a name without a process ID.
func TestRemoveAbandonedKeepsEveryLiveOrUnknownDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("RemoveAbandoned removes nothing on Windows")
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	const prefix = "abandoned-test-"
	child := exec.Command(os.Args[0], "-test.run=^$")
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	dead := child.Process.Pid
	live := exec.Command(os.Args[0], "-test.run=^TestRemoveAbandonedHold$")
	live.Env = append(os.Environ(), "TESTUTIL_HOLD=1")
	stdin, err := live.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = live.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = live.Wait() })
	names := map[string]bool{
		fmt.Sprintf("%s%d-aaa", prefix, dead):             false,
		fmt.Sprintf("%s%d-aaa", prefix, live.Process.Pid): true,
		fmt.Sprintf("%s%d-aaa", prefix, os.Getpid()):      true,
		prefix + "notanumber-aaa":                         true,
		fmt.Sprintf("other-%d-aaa", dead):                 true,
	}
	for name := range names {
		if err = os.Mkdir(filepath.Join(tmp, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	RemoveAbandoned(prefix)
	for name, kept := range names {
		_, err = os.Stat(filepath.Join(tmp, name))
		if kept && err != nil {
			t.Errorf("%s was removed: %v", name, err)
		}
		if !kept && err == nil {
			t.Errorf("%s of an exited process was kept", name)
		}
	}
}

// TestRemoveAbandonedHold is the process the test above keeps alive: it
// waits until its standard input closes.
func TestRemoveAbandonedHold(t *testing.T) {
	if os.Getenv("TESTUTIL_HOLD") == "" {
		t.Skip("run only as the helper process")
	}
	_, _ = os.Stdin.Read(make([]byte, 1))
}
