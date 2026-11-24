package nativepty

import (
	"errors"
	"os"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestScanGroup(t *testing.T) {
	const group, statPath = 100, "/proc/1/stat"
	member := []byte("5 (agent) S 1 100 100 0 -1\n")
	other := []byte("6 (other) S 1 200 200 0 -1\n")
	zombie := []byte("7 (agent) Z 1 100 100 0 -1\n")
	// A command name may hold parentheses and spaces.
	odd := []byte("8 (a) b (c) S 1 100 100 0 -1\n")
	stat := func(m map[string][]byte, errs map[string]error) func(string) ([]byte, error) {
		return func(pid string) ([]byte, error) { return m[pid], errs[pid] }
	}
	tests := []struct {
		name    string
		pids    []string
		data    map[string][]byte
		errs    map[string]error
		gone    bool
		wantErr string
	}{
		{name: "no pids", gone: true},
		{name: "ESRCH at read is skipped", pids: []string{"1", "6"}, gone: true,
			errs: map[string]error{"1": &os.PathError{Op: "read", Path: statPath, Err: unix.ESRCH}},
			data: map[string][]byte{"6": other}},
		{name: "ENOENT at open is skipped", pids: []string{"1", "6"}, gone: true,
			errs: map[string]error{"1": &os.PathError{Op: "open", Path: statPath, Err: unix.ENOENT}},
			data: map[string][]byte{"6": other}},
		{name: "empty read is skipped", pids: []string{"1", "6"}, gone: true,
			data: map[string][]byte{"1": {}, "6": other}},
		{name: "member after a vanished pid", pids: []string{"1", "5"},
			errs: map[string]error{"1": unix.ESRCH}, data: map[string][]byte{"5": member}},
		{name: "member present", pids: []string{"6", "5"}, data: map[string][]byte{"5": member, "6": other}},
		{name: "member with odd name", pids: []string{"8"}, data: map[string][]byte{"8": odd}},
		{name: "zombie is stopped", pids: []string{"7"}, gone: true, data: map[string][]byte{"7": zombie}},
		{name: "permission denied fails", pids: []string{"1", "5"}, wantErr: "permission denied",
			errs: map[string]error{"1": &os.PathError{Op: "open", Path: statPath, Err: unix.EACCES}},
			data: map[string][]byte{"5": member}},
		{name: "I/O error fails", pids: []string{"1"}, wantErr: "input/output error",
			errs: map[string]error{"1": unix.EIO}},
		{name: "malformed stat fails", pids: []string{"1"}, wantErr: "invalid process status",
			data: map[string][]byte{"1": []byte("garbage")}},
		{name: "truncated stat fails", pids: []string{"1"}, wantErr: "incomplete process status",
			data: map[string][]byte{"1": []byte("1 (x) S 1")}},
		{name: "non-numeric group fails", pids: []string{"1"}, wantErr: "invalid syntax",
			data: map[string][]byte{"1": []byte("1 (x) S 1 abc")}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gone, err := scanGroup(group, test.pids, stat(test.data, test.errs))
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) || gone {
					t.Fatalf("scanGroup() = %v, %v, want an error containing %q", gone, err, test.wantErr)
				}
				return
			}
			if err != nil || gone != test.gone {
				t.Fatalf("scanGroup() = %v, %v, want %v, nil", gone, err, test.gone)
			}
		})
	}
	if _, err := scanGroup(group, []string{"1"}, func(string) ([]byte, error) {
		return nil, errors.New("boom")
	}); err == nil {
		t.Fatal("an unknown error must fail the check")
	}
}
