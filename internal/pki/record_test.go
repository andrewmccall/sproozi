package pki

import (
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// buildStates builds every state in outcomeStates once, keyed by name, each
// in its own root.
func buildStates(t *testing.T) map[string]string {
	t.Helper()
	roots := map[string]string{}
	for _, state := range outcomeStates() {
		root := t.TempDir()
		state.build(t, outcomeStore(t, root))
		roots[state.name] = root
	}
	return roots
}

// refusedRecordErrors maps each corruption no phase describes to the error
// load names it with.
func refusedRecordErrors() map[string]string {
	return map[string]string{
		corruptVersion2:                    "version 2 is not 1",
		"no-generations":                   "no generations",
		"trusted-empty":                    "0 trusted generations",
		"serving-unknown":                  "serving generation 9 is not recorded",
		"serving-retired":                  "serving generation 1 is retired",
		"trusted-unknown":                  "trusted generation 9 is not recorded",
		"trusted-duplicate":                "generation 1 is trusted twice",
		"trusted-retired":                  "trusted generation 1 is retired",
		"serving-not-trusted":              "generation 1 is neither trusted nor retired",
		"generation-duplicate":             "generation 1 is listed where generation 2 belongs",
		"generation-zero":                  "generation 0 is listed where generation 1 belongs",
		"generation-gap":                   "generation 3 is listed where generation 2 belongs",
		"generation-not-retired-untrusted": "generation 2 is neither trusted nor retired",
		"fingerprint-empty":                "generation 1 has no SHA-256 fingerprint",
		"not-after-zero":                   "generation 1 has no expiry",
		"stage-unknown":                    `rotation stage "bogus" is not prepared or switched`,
		"stage-empty":                      `rotation stage "" is not prepared or switched`,
		"rotation-same-generation":         "rotation from 1 to 1 does not match the trusted generations [1 2]",
		"rotation-unknown-generation":      "rotation from 1 to 9 does not match the trusted generations [1 2]",
		"rotation-reversed":                "rotation from 2 to 1 does not match the trusted generations [1 2]",
		"rotation-prepared-at-zero":        "rotation has no preparation time",
		"prepared-serving-new":             "rotation stage prepared does not match serving generation 2",
		"switched-serving-old":             "rotation stage switched does not match serving generation 1",
		"switched-stage-prepared":          "rotation stage prepared does not match serving generation 2",
		"prepared-trusted-reversed":        "rotation from 1 to 2 does not match the trusted generations [2 1]",
		"prepared-trusted-one":             "generation 2 is neither trusted nor retired",
		"rotation-dropped":                 "generations 1 and 2 are trusted with no rotation recorded",
		"rotation-after-retire":            "rotation switched from 1 to 2 is recorded, but only generation 2 is trusted",
		"retired-unmarked":                 "generation 1 is neither trusted nor retired",
		"leaf-outside":                     `leaf name "../victim" is not a leaf file name`,
		"trusted-three":                    "3 trusted generations; a record trusts one, or two during a rotation",
		"rotation-target-not-newest":       "rotation target 2 is not the newest generation",
		"retired-fingerprint-garbage":      "generation 1 has no SHA-256 fingerprint",
	}
}

// refusedRecordCases are corruptions() plus two hand edits that add a third
// generation. The two are not in the recorded table, which was generated
// before they were added.
func refusedRecordCases() []corruption {
	third := func(retired bool) Generation {
		return Generation{ID: 3, Fingerprint: strings.Repeat("cd", 32), NotAfter: outcomeStart, Retired: retired}
	}
	return append(corruptions(),
		corruption{"trusted-three", stPrepared, editState(func(st *State) {
			st.Generations, st.Trusted = append(st.Generations, third(false)), []int{1, 2, 3}
		})},
		corruption{"rotation-target-not-newest", stPrepared, editState(func(st *State) {
			st.Generations = append(st.Generations, third(true))
		})},
		corruption{"retired-fingerprint-garbage", stRetired, editState(func(st *State) {
			st.Generations[0].Fingerprint = "zz"
		})})
}

// A record whose structure no phase describes is refused with an error
// naming what is wrong by load and by every request but replace: it is never
// repaired, never a new authority on its own, and nothing in the store
// changes. Guards describe in record.go (each case removes one rule) and the
// refusal of phaseRefused by every event but replace in transitions.go.
func TestOnlyReplaceProceedsOnARefusedRecord(t *testing.T) {
	refusals := refusedRecordErrors()
	roots := buildStates(t)
	seen := map[string]bool{}
	for _, c := range refusedRecordCases() {
		want, ok := refusals[c.name]
		if !ok {
			continue
		}
		seen[c.name] = true
		t.Run(c.name, func(t *testing.T) {
			root := copyStore(t, filepath.Join(roots[c.from], "pki"))
			c.apply(t, filepath.Join(root, "pki"))
			before := storeFiles(t, filepath.Join(root, "pki"))
			s := outcomeStore(t, root)
			for _, ev := range outcomeEvents() {
				if ev.name == evNameReplace {
					continue
				}
				err := ev.run(s)
				if err == nil || !strings.Contains(err.Error(), "pki: corrupt state: "+want) {
					t.Errorf("%s: got %v, want refusal %q", ev.name, err, want)
				}
				if got := storeFiles(t, filepath.Join(root, "pki")); !maps.Equal(got, before) {
					t.Errorf("refused %s changed the store", ev.name)
				}
			}
			if c.name == corruptVersion2 {
				// A newer version's record: replace refuses it too.
				if err := s.ReplaceCA(); err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("replace of a version 2 record: %v", err)
				}
				return
			}
			if err := s.ReplaceCA(); err != nil {
				t.Errorf("replace: %v", err)
			}
		})
	}
	for name := range refusals {
		if !seen[name] {
			t.Errorf("no corruption named %s", name)
		}
	}
}

// storeFiles reads every file in dir by name.
func storeFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	must(t, err)
	files := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || e.Name() == lockFile {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		must(t, err)
		files[e.Name()] = string(data)
	}
	return files
}

// Every record the code writes, and every record an interrupted write
// leaves, is described by a phase and loads.
func TestEveryWrittenRecordLoads(t *testing.T) {
	for name, root := range buildStates(t) {
		if name == stEmpty || name == stStateLost {
			continue
		}
		s := outcomeStore(t, root)
		if _, err := s.State(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// A record whose trusted generations' certificates are not the ones it
// names is refused on load, naming the file. Keys are not read on load:
// see TestAKeyIsCheckedWhereItIsUsed. Replace itself reads only the record,
// so it still recovers from every case here. Guards checkFiles in
// record.go.
func TestLoadRefusesAuthorityFilesTheRecordDoesNotName(t *testing.T) {
	refusals := map[string]string{
		"fingerprint-other":   msgFingerprint1,
		"ca-cert-missing":     "generation 1 certificate ca-1.pem is unreadable",
		"new-ca-cert-missing": "generation 2 certificate ca-2.pem is unreadable",
		"ca-cert-other":       msgFingerprint1,
		"ca-pair-other":       msgFingerprint1,
		"not-after-other":     "generation 1 certificate ca-1.pem does not have the recorded expiry",
		// A retired generation's kept certificate is compared too.
		"retired-fingerprint-other": msgFingerprint1,
		"retired-not-after-other":   "generation 1 certificate ca-1.pem does not have the recorded expiry",
		// A blank history entry is accepted only where its certificate is gone.
		"retired-blank-with-cert": "generation 1 certificate ca-1.pem is kept, but the record has no fingerprint or expiry for it",
	}
	cases := append(corruptions(),
		corruption{"not-after-other", stSingle, editState(func(st *State) {
			st.Generations[0].NotAfter = st.Generations[0].NotAfter.Add(-24 * time.Hour)
		})},
		corruption{"retired-fingerprint-other", stRetired, editState(func(st *State) {
			st.Generations[0].Fingerprint = strings.Repeat("ab", 32)
		})},
		corruption{"retired-not-after-other", stRetired, editState(func(st *State) {
			st.Generations[0].NotAfter = st.Generations[0].NotAfter.Add(-24 * time.Hour)
		})},
		corruption{"retired-blank-with-cert", stRetired, editState(func(st *State) {
			st.Generations[0].Fingerprint, st.Generations[0].NotAfter = "", time.Time{}
		})})
	roots := buildStates(t)
	seen := map[string]bool{}
	for _, c := range cases {
		want, ok := refusals[c.name]
		if !ok {
			continue
		}
		seen[c.name] = true
		t.Run(c.name, func(t *testing.T) {
			root := copyStore(t, filepath.Join(roots[c.from], "pki"))
			c.apply(t, filepath.Join(root, "pki"))
			s := outcomeStore(t, root)
			for _, ev := range outcomeEvents() {
				if ev.name == evNameInitialize || ev.name == evNameReplace {
					continue
				}
				err := ev.run(s)
				if err == nil || !strings.Contains(err.Error(), "pki: certificate authority files do not match state: "+want) {
					t.Errorf("%s: got %v, want refusal %q", ev.name, err, want)
				}
			}
			if err := s.Initialize(testNames); err == nil || !strings.Contains(err.Error(), "already initialized") {
				t.Errorf("initialize: %v", err)
			}
			if err := s.ReplaceCA(); err != nil {
				t.Errorf("replace: %v", err)
			}
			if _, err := s.State(); err != nil {
				t.Errorf("load after replace: %v", err)
			}
		})
	}
	for name := range refusals {
		if !seen[name] {
			t.Errorf("no corruption named %s", name)
		}
	}
}
