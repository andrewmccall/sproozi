package pki

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A trusted generation's key is not read on load: a read of the serving
// leaf succeeds whenever the record and its certificates are valid. The
// operation about to use a key reports its true cause, and Ensure, after its
// leaf and trust work, reports every trusted key that is unreadable or does
// not match its certificate. A missing key is the lost-key state and is not
// reported. Guards checkKeys and its call in Ensure, and checkFiles not
// reading keys.
func TestAKeyIsCheckedWhereItIsUsed(t *testing.T) {
	roots := buildStates(t)
	other := func(t *testing.T) []byte {
		ca, err := NewCA("hand-made", outcomeStart, DefaultPolicy.CAValidity)
		must(t, err)
		return ca.Key
	}
	for _, tc := range []struct {
		name, from, key string
		edit            func(t *testing.T, path string)
		ensure, use     string
		use_            func(s *Store) error
	}{
		{"open to others", stPrepared, "ca-2.key", func(t *testing.T, path string) { must(t, os.Chmod(path, 0o644)) },
			"generation 2 key ca-2.key is unreadable: privatefs: path must be owned, private and ordinary",
			"pki: serving CA key unavailable; recover it or replace the CA: privatefs: path must be owned, private and ordinary",
			func(s *Store) error { return s.SwitchRotation(true) }},
		{"another key", stPrepared, "ca-2.key", func(t *testing.T, path string) { must(t, os.WriteFile(path, other(t), 0o600)) },
			"generation 2 key ca-2.key does not match its certificate: pki: private key does not match certificate",
			"pki: private key does not match certificate",
			func(s *Store) error { return s.SwitchRotation(true) }},
		{"garbage", stSingle, "ca-1.key", func(t *testing.T, path string) { must(t, os.WriteFile(path, []byte("not a key\n"), 0o600)) },
			"generation 1 key ca-1.key does not match its certificate: pki: one PKCS#8 private key required",
			"pki: one PKCS#8 private key required",
			func(s *Store) error { return s.Renew() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := copyStore(t, filepath.Join(roots[tc.from], "pki"))
			tc.edit(t, filepath.Join(root, "pki", tc.key))
			s := outcomeStore(t, root)
			if _, _, _, err := s.Serving(); err != nil {
				t.Errorf("serving: %v", err)
			}
			if _, err := s.State(); err != nil {
				t.Errorf("load: %v", err)
			}
			if _, err := s.Ensure(testNames); !errors.Is(err, ErrOutstanding) ||
				!strings.Contains(err.Error(), "pki: certificate authority key: "+tc.ensure) {
				t.Errorf("ensure: %v, want outstanding %q", err, tc.ensure)
			}
			if err := tc.use_(s); err == nil || !strings.Contains(err.Error(), tc.use) {
				t.Errorf("use: %v, want %q", err, tc.use)
			}
		})
	}
}

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
