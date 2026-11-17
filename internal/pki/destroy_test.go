package pki

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func keyFiles(t *testing.T, s *Store) []string {
	t.Helper()
	entries, err := os.ReadDir(s.Dir())
	must(t, err)
	var keys []string
	for _, e := range entries {
		if m := caFileRE.FindStringSubmatch(e.Name()); m != nil && m[2] == "key" {
			keys = append(keys, e.Name())
		}
	}
	return keys
}

// A retired generation's key that a crash or a failed removal left behind
// is destroyed by the next Ensure, which the certificate controller runs on
// every reconcile and the local service at start and every minute. Before,
// nothing destroyed it again. Guards the destroyRetired call in Ensure.
func TestEnsureDestroysARetiredKeyLeftBehind(t *testing.T) {
	roots := buildStates(t)
	for name, want := range map[string][]string{
		"retired-key-left":        {keyCA2},
		"replaced-key-left":       {keyCA2},
		stReplacedKeyLeftPrepared: {keyCA2, "ca-3.key"},
	} {
		s := outcomeStore(t, copyStore(t, filepath.Join(roots[name], "pki")))
		if _, err := s.Ensure(testNames); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := keyFiles(t, s); !slices.Equal(got, want) {
			t.Errorf("%s: keys %v, want %v", name, got, want)
		}
	}
}

// Ensure never destroys a key some phase still needs: the key of every
// trusted generation survives it in every state, and only those keys do: a
// retired generation's, and an uncommitted generation's that an interrupted
// prepare or replace wrote, are destroyed. Guards the Retired test and the
// uncommitted-generation loop in destroyRetired.
func TestEnsureKeepsEveryKeyAPhaseNeeds(t *testing.T) {
	for name, root := range buildStates(t) {
		if name == stEmpty || name == stStateLost {
			continue
		}
		s := outcomeStore(t, copyStore(t, filepath.Join(root, "pki")))
		st, err := s.State()
		must(t, err)
		var needed []string
		for _, key := range keyFiles(t, s) {
			var id int
			_, _ = fmt.Sscan(caFileRE.FindStringSubmatch(key)[1], &id)
			if g := st.generation(id); g != nil && !g.Retired {
				needed = append(needed, key)
			}
		}
		if _, err = s.Ensure(testNames); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := keyFiles(t, s); !slices.Equal(got, needed) {
			t.Errorf("%s: keys %v after Ensure, want %v", name, got, needed)
		}
	}
}

// A leaf key the record does not name, left by a renewal that stopped after
// its commit or before it, is destroyed by the next Ensure; the named leaf
// is kept. Guards the leaf loop in destroyRetired.
func TestEnsureDestroysLeafFilesTheRecordDoesNotName(t *testing.T) {
	roots := buildStates(t)
	s := outcomeStore(t, copyStore(t, filepath.Join(roots["superseded-leaf-left"], "pki")))
	st, err := s.State()
	must(t, err)
	// An uncommitted renewal's key, written before its certificate.
	must(t, os.WriteFile(filepath.Join(s.Dir(), "leaf-0123456789abcdef.key"), []byte("uncommitted\n"), 0o600))
	if _, err = s.Ensure(testNames); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(s.Dir())
	must(t, err)
	var leaves []string
	for _, e := range entries {
		if leafFile.MatchString(e.Name()) {
			leaves = append(leaves, e.Name())
		}
	}
	if want := []string{st.Leaf + ".key", st.Leaf + ".pem"}; !slices.Equal(leaves, want) {
		t.Errorf("leaf files %v, want %v", leaves, want)
	}
}

// The files of a generation a prepare or replace wrote and never committed
// are destroyed by the next Ensure, certificate and key, so the controller
// no longer copies them into the authority Secret. Names the store does not
// write are left, whatever they look like. Guards the uncommitted-generation
// loop in destroyRetired and caFilePattern.
func TestEnsureDestroysAnUncommittedGeneration(t *testing.T) {
	roots := buildStates(t)
	s := outcomeStore(t, copyStore(t, filepath.Join(roots["uncommitted-generation"], "pki")))
	for _, name := range foreignFiles {
		must(t, os.WriteFile(filepath.Join(s.Dir(), name), []byte("not the store's\n"), 0o600))
	}
	if _, err := s.Ensure(testNames); err != nil {
		t.Fatal(err)
	}
	files := storeFiles(t, s.Dir())
	for _, name := range []string{keyCA2, "ca-2.pem"} {
		if _, kept := files[name]; kept {
			t.Errorf("%s survived Ensure", name)
		}
	}
	for _, name := range append([]string{keyCA1, "ca-1.pem"}, foreignFiles...) {
		if _, kept := files[name]; !kept {
			t.Errorf("%s was removed", name)
		}
	}
}

// A retirement whose key removal fails reports it, and the next Ensure
// destroys the key. Guards the destroyRetired call in RetireRotation and in
// Ensure.
func TestAFailedRetiredKeyRemovalIsRetried(t *testing.T) {
	roots := buildStates(t)
	s := outcomeStore(t, copyStore(t, filepath.Join(roots[stSwitched], "pki")))
	s.beforeWrite = func(op, name string) error {
		if op == "remove" && name == keyCA1 {
			return errStopped
		}
		return nil
	}
	if err := s.RetireRotation(true); !errors.Is(err, errStopped) {
		t.Fatalf("retire with a failing removal: %v", err)
	}
	if _, err := s.Ensure(testNames); !errors.Is(err, errStopped) {
		t.Fatalf("ensure with a failing removal: %v", err)
	}
	s.beforeWrite = nil
	if _, err := s.Ensure(testNames); err != nil {
		t.Fatal(err)
	}
	if got := keyFiles(t, s); !slices.Equal(got, []string{keyCA2}) {
		t.Errorf("keys %v, want only ca-2.key", got)
	}
}
