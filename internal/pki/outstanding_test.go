package pki

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A destroy pass that cannot remove a file does not stop Ensure from doing
// its work: the due leaf is renewed and the missing trust bundle written,
// and only then is the failed removal reported, so the next Ensure retries
// it. Guards the order in Ensure: leaf and trust work before the destroy
// pass's error is returned.
func TestAFailedRemovalDoesNotBlockRenewal(t *testing.T) {
	roots := buildStates(t)
	root := copyStore(t, filepath.Join(roots[stSingle], "pki"))
	dir := filepath.Join(root, "pki")
	// A non-empty directory with a leaf file's name: the pass tries to
	// remove it and fails.
	stray := filepath.Join(dir, "leaf-0000000000000000.key")
	must(t, os.Mkdir(stray, 0o700))
	must(t, os.WriteFile(filepath.Join(stray, "x"), []byte("x"), 0o600))
	must(t, os.Remove(filepath.Join(dir, trustFile)))
	s := outcomeStore(t, root)
	before, err := s.State()
	must(t, err)
	cert, err := s.loadLeaf(before)
	must(t, err)
	clk := &clock{t: DefaultPolicy.RenewAt(cert.Leaf).Add(1)}
	s.now = clk.now
	changed, err := s.Ensure(testNames)
	if !errors.Is(err, ErrOutstanding) || !strings.Contains(err.Error(), "pki: destroy retired keys:") {
		t.Errorf("ensure did not report the failed removal: %v", err)
	}
	after, err := s.State()
	must(t, err)
	if !changed || after.Leaf == before.Leaf {
		t.Errorf("ensure did not renew the leaf: changed=%v", changed)
	}
	if _, err = os.Stat(filepath.Join(dir, trustFile)); err != nil {
		t.Errorf("ensure did not write the trust bundle: %v", err)
	}
	if _, err = os.Stat(stray); err != nil {
		t.Errorf("the stray directory: %v", err)
	}
}
