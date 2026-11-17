package pki

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Save refuses a record no phase describes, or whose certificates are not
// the ones it names, and writes nothing. Every mutator builds a valid record,
// so only this test reaches the check. Guards the check at the top of
// commit (reviewer's M19b).
func TestSaveRefusesARecordLoadWouldRefuse(t *testing.T) {
	roots := buildStates(t)
	for _, tc := range []struct {
		name string
		edit func(st *State)
		want string
	}{
		{"no phase", func(st *State) { st.Rotation = &Rotation{Stage: "bogus", From: 1, To: 2, PreparedAt: outcomeStart} },
			`refusing to save: pki: corrupt state: rotation bogus from 1 to 2 is recorded`},
		{"another fingerprint", func(st *State) { st.Generations[0].Fingerprint = strings.Repeat("ab", 32) },
			"refusing to save: pki: certificate authority files do not match state: " + msgFingerprint1},
	} {
		root := copyStore(t, filepath.Join(roots[stSingle], "pki"))
		s := outcomeStore(t, root)
		before := storeFiles(t, s.Dir())
		st, err := s.State()
		must(t, err)
		tc.edit(&st)
		if err = s.commit(st); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: commit %v, want %q", tc.name, err, tc.want)
		}
		if after := storeFiles(t, s.Dir()); !equalFiles(before, after) {
			t.Errorf("%s: a refused save wrote the store", tc.name)
		}
	}
}

func equalFiles(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// The destroy pass removes only leaf-<16 hex>.pem and .key: an entry with
// that stem and no extension, or another extension, is not the store's and
// stays. Guards the extension test in destroyRetired (reviewer's M27).
func TestTheDestroyPassKeepsLeafNamesWithoutTheStoresExtensions(t *testing.T) {
	roots := buildStates(t)
	s := outcomeStore(t, copyStore(t, filepath.Join(roots[stSingle], "pki")))
	kept := []string{"leaf-0123456789abcdef", "leaf-0123456789abcdef.txt", "leaf-0123456789abcdef.key.bak"}
	for _, name := range kept {
		must(t, os.WriteFile(filepath.Join(s.Dir(), name), []byte("not the store's\n"), 0o600))
	}
	must(t, os.Mkdir(filepath.Join(s.Dir(), "leaf-fedcba9876543210"), 0o700))
	if _, err := s.Ensure(testNames); err != nil {
		t.Fatal(err)
	}
	for _, name := range append(kept, "leaf-fedcba9876543210") {
		if _, err := os.Stat(filepath.Join(s.Dir(), name)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// apply refuses a move whose result is not the phase the table declares,
// and leaves the record unchanged: the table and apply's edits must agree.
// Guards the describe check at the end of apply (reviewer's M25b).
func TestApplyRefusesAResultTheTableDoesNotDeclare(t *testing.T) {
	roots := buildStates(t)
	s := outcomeStore(t, copyStore(t, filepath.Join(roots[stPrepared], "pki")))
	st, err := s.State()
	must(t, err)
	before := st.clone()
	transitions[evSwitch][PhasePrepared] = PhaseSingle
	defer func() { transitions[evSwitch][PhasePrepared] = PhaseSwitched }()
	if err = st.apply(evSwitch, nil, outcomeStart); err == nil || !strings.Contains(err.Error(), "did not produce single") {
		t.Errorf("apply: %v", err)
	}
	if st.Serving != before.Serving || st.Rotation.Stage != before.Rotation.Stage {
		t.Error("a refused apply changed the record")
	}
}
