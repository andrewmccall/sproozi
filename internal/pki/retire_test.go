package pki

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every step that signs with the serving key reports a broken one the same
// way, naming the generation, the file and the cause, whether the key
// cannot be read, is not a key, or belongs to another authority. Guards
// servingPair's use in issue.
func TestABrokenServingKeyIsReportedTheSameWayEverywhere(t *testing.T) {
	roots := buildStates(t)
	other, err := NewCA("hand-made", outcomeStart, DefaultPolicy.CAValidity)
	must(t, err)
	const unavailable = "pki: serving CA key unavailable; recover it or replace the CA: "
	for _, tc := range []struct {
		name, from, key string
		data            []byte
		run             func(s *Store) error
		want            string
	}{
		{"renew, garbage", stSingle, keyCA1, []byte("not a key\n"), func(s *Store) error { return s.Renew() },
			unavailable + "generation 1 key ca-1.key: pki: one PKCS#8 private key required"},
		{"renew, another key", stSingle, keyCA1, other.Key, func(s *Store) error { return s.Renew() },
			unavailable + "generation 1 key ca-1.key: pki: private key does not match certificate"},
		{"switch, garbage", stPrepared, keyCA2, []byte("not a key\n"), func(s *Store) error { return s.SwitchRotation(true) },
			unavailable + "generation 2 key ca-2.key: pki: one PKCS#8 private key required"},
		{"switch, another key", stPrepared, keyCA2, other.Key, func(s *Store) error { return s.SwitchRotation(true) },
			unavailable + "generation 2 key ca-2.key: pki: private key does not match certificate"},
	} {
		root := copyStore(t, filepath.Join(roots[tc.from], "pki"))
		must(t, os.WriteFile(filepath.Join(root, "pki", tc.key), tc.data, 0o600))
		if err := tc.run(outcomeStore(t, root)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

// Retire destroys the outgoing key only after proving the serving key works
// for its recorded certificate. With the new serving key broken, retire is
// refused naming it, the store stays switched and the outgoing key, the one
// that still works, is kept so rollback remains. Guards the serving-key
// check in RetireRotation.
func TestRetireKeepsTheLastWorkingKey(t *testing.T) {
	roots := buildStates(t)
	other := func(t *testing.T) []byte {
		ca, err := NewCA("hand-made", outcomeStart, DefaultPolicy.CAValidity)
		must(t, err)
		return ca.Key
	}
	for name, key := range map[string]func(t *testing.T) []byte{
		"garbage":     func(*testing.T) []byte { return []byte("not a key\n") },
		"another key": other,
	} {
		t.Run(name, func(t *testing.T) {
			root := copyStore(t, filepath.Join(roots[stSwitched], "pki"))
			dir := filepath.Join(root, "pki")
			must(t, os.WriteFile(filepath.Join(dir, keyCA2), key(t), 0o600))
			s := outcomeStore(t, root)
			err := s.RetireRotation(true)
			if err == nil || !strings.Contains(err.Error(), "generation 2 key ca-2.key") {
				t.Errorf("retire: %v, want a refusal naming ca-2.key", err)
			}
			if _, err = os.Stat(filepath.Join(dir, keyCA1)); err != nil {
				t.Errorf("the outgoing key: %v", err)
			}
			if st, _ := s.State(); st.Phase() != PhaseSwitched {
				t.Errorf("phase %s, want switched", st.Phase())
			}
			if err = s.RollbackRotation(); err != nil {
				t.Errorf("rollback: %v", err)
			}
		})
	}
}
