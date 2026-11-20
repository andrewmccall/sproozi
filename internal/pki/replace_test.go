package pki

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"testing"
)

// Files a store never writes, and one outside it. Replace leaves them all.
var foreignFiles = []string{"ca-1.key.bak", "ca-01.key", "ca-09.key", "ca-009.pem", "ca-x.key", "leaf-zz.key", "leaf-0123.key", "notes.txt"}

var caNumber = regexp.MustCompile(`^ca-([1-9][0-9]*)\.(pem|key)$`)

// An explicit replace is the way out of a record no phase describes. It
// leaves a single phase on a generation numbered after every CA file and
// record entry, trusting only it; every earlier CA key and every leaf file
// but the new leaf is destroyed; every earlier CA certificate is kept
// byte-for-byte; files the store does not write, and files outside it, are
// untouched. Guards replaceable in record.go and the phaseRefused row of
// evReplace in transitions.go.
func TestReplaceRecoversARefusedRecord(t *testing.T) {
	t.Parallel() // its durable writes wait on the disk; see sharedStates
	refusals := refusedRecordErrors()
	roots := buildStates(t)
	for _, c := range refusedRecordCases() {
		if _, ok := refusals[c.name]; !ok || c.name == corruptVersion2 {
			continue
		}
		t.Run(c.name, func(t *testing.T) {
			root := copyStore(t, filepath.Join(roots[c.from], "pki"))
			dir := filepath.Join(root, "pki")
			c.apply(t, dir)
			for _, name := range foreignFiles {
				must(t, os.WriteFile(filepath.Join(dir, name), []byte("not the store's\n"), 0o600))
			}
			before := storeFiles(t, dir)
			highest := 0
			for name := range before {
				if m := caNumber.FindStringSubmatch(name); m != nil {
					var id int
					_, _ = fmt.Sscan(m[1], &id)
					highest = max(highest, id)
				}
			}
			s := outcomeStore(t, root)
			if err := s.ReplaceCA(); err != nil {
				t.Fatalf("replace: %v", err)
			}
			st, err := s.State()
			if err != nil {
				t.Fatalf("load after replace: %v", err)
			}
			next := len(st.Generations)
			if st.Phase() != PhaseSingle || st.Serving != next || !slices.Equal(st.Trusted, []int{next}) || next <= highest {
				t.Errorf("replace left serving %d trusted %v of %d generations (phase %q), highest file ca-%d",
					st.Serving, st.Trusted, next, st.Phase(), highest)
			}
			for _, g := range st.Generations[:next-1] {
				if !g.Retired {
					t.Errorf("generation %d is not retired", g.ID)
				}
			}
			after := storeFiles(t, dir)
			for name, data := range before {
				m := caNumber.FindStringSubmatch(name)
				switch {
				case m != nil && m[2] == "key" || leafFile.MatchString(name):
					if _, kept := after[name]; kept {
						t.Errorf("%s survived replace", name)
					}
				case m != nil || slices.Contains(foreignFiles, name):
					if after[name] != data {
						t.Errorf("%s changed", name)
					}
				}
			}
			for name := range after {
				if leafFile.MatchString(name) && name != st.Leaf+".pem" && name != st.Leaf+".key" {
					t.Errorf("%s is a leaf file the record does not name", name)
				}
			}
			for _, name := range victimFiles {
				if _, err := os.Stat(filepath.Join(root, name)); err != nil {
					t.Errorf("outside the store: %v", err)
				}
			}
		})
	}
}
