package testutil

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// FixtureDir creates a temporary directory whose name is prefix, this
// process's ID and a random suffix, so that RemoveAbandoned can tell whose it
// was. It is for a fixture a TestMain removes, which a killed or timed-out
// test binary leaves behind.
func FixtureDir(prefix string) (string, error) {
	return os.MkdirTemp("", fmt.Sprintf("%s%d-", prefix, os.Getpid()))
}

// RemoveAbandoned removes the FixtureDir directories of prefix that earlier
// runs left, as a run killed by its timeout does. It removes a directory only
// when signalling its process reports that the process is gone, so a
// directory of a run still going, in this or another checkout, of a reused
// process ID or of another user's process is kept; where that cannot be
// told, as on Windows, nothing is removed.
func RemoveAbandoned(prefix string) {
	dirs, _ := filepath.Glob(filepath.Join(os.TempDir(), prefix+"*-*"))
	for _, dir := range dirs {
		pidText, _, ok := strings.Cut(strings.TrimPrefix(filepath.Base(dir), prefix), "-")
		pid, err := strconv.Atoi(pidText)
		if !ok || err != nil || pid <= 0 || pid == os.Getpid() {
			continue
		}
		process, err := os.FindProcess(pid)
		if err != nil {
			continue
		}
		if errors.Is(process.Signal(syscall.Signal(0)), os.ErrProcessDone) {
			_ = os.RemoveAll(dir)
		}
	}
}
