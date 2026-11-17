package pki

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Replace on a refused record is never driven past what load can read by a
// stray ca-N file name or a generation id in the refused record: it refuses,
// naming the file or id, and writes nothing. Once the stray file is removed,
// replace succeeds and the store loads. Guards the bound in replaceable
// (reviewer's TestR2ReplaceSize and mutant N06).
func TestReplaceIsNotDrivenPastWhatLoadReads(t *testing.T) {
	roots := buildStates(t)
	for _, tc := range []struct {
		name, stray, want string
		edit              func(st *State)
	}{
		{"stray ca-900.pem, refused record", "ca-900.pem", "after ca-900.pem",
			func(st *State) { st.Serving = 9 }},
		{"stray ca-9999.key, certificate mismatch", "ca-9999.key", "after ca-9999.key",
			func(st *State) { st.Generations[0].Fingerprint = strings.Repeat("ab", 32) }},
		{"stray ca-10001.pem, refused record", "ca-10001.pem", "after ca-10001.pem",
			func(st *State) { st.Serving = 9 }},
		// Beyond maxState/minGenerationBytes, refused before the history is
		// built: a name like ca-999999999.pem must not build a billion
		// entries.
		{"stray ca-2000.pem, refused before building", "ca-2000.pem", "after ca-2000.pem, and a record of 2001 generations is more than",
			func(st *State) { st.Serving = 9 }},
		// Within it, refused by the measured size.
		{"stray ca-900.pem, measured", "ca-900.pem", "after ca-900.pem, and a record of 901 generations is 1",
			func(st *State) { st.Serving = 9 }},
		{"generation id 5000 in the record", "", "after generation 5000 in state.json",
			func(st *State) { st.Generations[0].ID = 5000 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := copyStore(t, filepath.Join(roots[stSingle], "pki"))
			dir := filepath.Join(root, "pki")
			if tc.stray != "" {
				must(t, os.WriteFile(filepath.Join(dir, tc.stray), nil, 0o600))
			}
			editState(tc.edit)(t, dir)
			before := storeFiles(t, dir)
			s := outcomeStore(t, root)
			err := s.ReplaceCA()
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "load reads") {
				t.Fatalf("replace: %v, want a refusal naming %q", err, tc.want)
			}
			if after := storeFiles(t, dir); !equalFiles(before, after) {
				t.Error("a refused replace wrote the store")
			}
			if tc.stray == "" {
				return
			}
			must(t, os.Remove(filepath.Join(dir, tc.stray)))
			if err = s.ReplaceCA(); err != nil {
				t.Fatalf("replace after removing %s: %v", tc.stray, err)
			}
			if _, err = s.State(); err != nil {
				t.Errorf("load after replace: %v", err)
			}
		})
	}
}

// No operation writes a record larger than load reads: an operation that
// would cross the bound is refused, naming the size, and writes nothing.
// Guards the size check in commit and in grow.
func TestNoOperationWritesARecordLoadCannotRead(t *testing.T) {
	roots := buildStates(t)
	root := copyStore(t, filepath.Join(roots[stSingle], "pki"))
	dir := filepath.Join(root, "pki")
	// A long history of retired generations, as many rotations would leave,
	// just short of the bound.
	editState(func(st *State) {
		live := st.Generations[0]
		st.Generations = nil
		for id := 1; ; id++ {
			retired := live
			retired.ID, retired.Retired, retired.Fingerprint = id, true, strings.Repeat("ab", 32)
			serving := live
			serving.ID = id + 1
			st.Generations = append(st.Generations, retired)
			st.Serving, st.Trusted = id+1, []int{id + 1}
			whole := *st
			whole.Generations = append(slices.Clone(st.Generations), serving)
			if data, _ := json.MarshalIndent(whole, "", "  "); len(data)+1 > maxState-200 {
				*st = whole
				break
			}
		}
	})(t, dir)
	s := outcomeStore(t, root)
	st, err := s.load()
	if err != nil {
		t.Skipf("history setup: %v", err)
	}
	// Rename ca-1 to the serving generation's files.
	n := len(st.Generations)
	for _, ext := range []string{".pem", ".key"} {
		must(t, os.Rename(filepath.Join(dir, "ca-1"+ext), filepath.Join(dir, caName(n)+ext)))
	}
	if _, err = s.State(); err != nil {
		t.Fatalf("the long history does not load: %v", err)
	}
	before := storeFiles(t, dir)
	for name, op := range map[string]func() error{
		"prepare": s.PrepareRotation,
		"replace": s.ReplaceCA,
	} {
		err := op()
		if err == nil || !strings.Contains(err.Error(), "load reads") {
			t.Errorf("%s: %v, want a refusal naming the bound", name, err)
		}
		if after := storeFiles(t, dir); !equalFiles(before, after) {
			t.Errorf("a refused %s wrote the store", name)
		}
	}
}
