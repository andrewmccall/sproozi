package nativepty

import (
	"bytes"
	"errors"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// scanGroup reports whether no process named in pids still runs in process
// group group. read returns one pid's /proc/<pid>/stat. It is a parameter, not
// a variable: only the Linux scan in group_linux.go supplies it, with the real
// /proc, and nothing in production configures it.
//
// A pid whose entry or stat content is gone because the process exited is not
// a member of any group and is skipped. That is ENOENT (the entry vanished
// before the open), ESRCH (it vanished between the open and the read; the
// kernel's proc_single_show returns it when the task is gone) and an empty
// read (inferred: an exiting process may leave nothing to read). Anything else
// fails the check: permission denied, an I/O error, or a stat line that
// cannot be parsed for a process that is still there. "Could not look" must
// never become "the group is gone", or a stop could be confirmed while a
// member still runs.
func scanGroup(group int, pids []string, read func(pid string) ([]byte, error)) (bool, error) {
	for _, pid := range pids {
		data, err := read(pid)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return false, err
		}
		if len(bytes.TrimSpace(data)) == 0 {
			continue
		}
		end := strings.LastIndexByte(string(data), ')')
		if end < 0 {
			return false, errors.New("invalid process status")
		}
		fields := strings.Fields(string(data[end+1:]))
		if len(fields) < 3 {
			return false, errors.New("incomplete process status")
		}
		processGroup, err := strconv.Atoi(fields[2])
		if err != nil {
			return false, err
		}
		if processGroup == group && fields[0] != "Z" && fields[0] != "X" {
			return false, nil
		}
	}
	return true, nil
}
