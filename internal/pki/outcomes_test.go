package pki

import (
	"bytes"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
)

var updateOutcomes = flag.Bool("update-outcomes", false, "rewrite testdata/store-outcomes.txt from the current code")

const outcomesFile = "testdata/store-outcomes.txt"

// Names of states, operations and files the store's tests share.
const (
	stEmpty                   = "empty"
	stSingle                  = "single"
	stPrepared                = "prepared"
	stSwitched                = "switched"
	stRetired                 = "retired"
	stStateLost               = "state-lost"
	stReplacedKeyLeftPrepared = "replaced-key-left-prepared"
	evNameInitialize          = "initialize"
	evNameSwitch              = "switch"
	evNameReplace             = "replace"
	keyCA2                    = "ca-2.key"
	keyCA1                    = "ca-1.key"
	msgFingerprint1           = "generation 1 certificate ca-1.pem does not have the recorded fingerprint"
	corruptVersion2           = "version-2"
)

// victimFiles sit beside the store's directory, outside it.
var victimFiles = []string{"victim.key", "victim.pem"}

// outcomeStart is the fixed clock every recorded outcome runs at.
var outcomeStart = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

// outcomeStore opens a store in root/pki. root also holds victim.key and
// victim.pem, outside the store, so an outcome that touches a file outside
// the store's directory is visible.
func outcomeStore(t *testing.T, root string) *Store {
	t.Helper()
	s, err := Open(filepath.Join(root, "pki"), DefaultPolicy, func() time.Time { return outcomeStart })
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range victimFiles {
		if _, err = os.Stat(filepath.Join(root, name)); os.IsNotExist(err) {
			if err = os.WriteFile(filepath.Join(root, name), []byte("outside the store\n"), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	return s
}

// copyStore copies a store's regular files into a fresh root.
func copyStore(t *testing.T, from string) string {
	t.Helper()
	root := t.TempDir()
	s := outcomeStore(t, root)
	entries, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() || entry.Name() == lockFile {
			continue
		}
		data, err := os.ReadFile(filepath.Join(from, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(s.Dir(), entry.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// keyBackup keeps a CA key's bytes so a state can put back a key that a
// crash would have left behind.
func keyBackup(t *testing.T, s *Store, id int) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.Dir(), caName(id)+".key"))
	must(t, err)
	return data
}

func restoreKey(t *testing.T, s *Store, id int, data []byte) {
	t.Helper()
	must(t, os.WriteFile(filepath.Join(s.Dir(), caName(id)+".key"), data, 0o600))
}

// outcomeState is a store state the recorded table starts from.
type outcomeState struct {
	name  string
	build func(t *testing.T, s *Store)
}

// outcomeStates are the states the code writes, and the states an
// interrupted operation leaves: the files it wrote before the process
// stopped.
func outcomeStates() []outcomeState {
	initialize := func(t *testing.T, s *Store) { must(t, s.Initialize(testNames)) }
	prepared := func(t *testing.T, s *Store) { initialize(t, s); must(t, s.PrepareRotation()) }
	switched := func(t *testing.T, s *Store) { prepared(t, s); must(t, s.SwitchRotation(true)) }
	return []outcomeState{
		{stEmpty, func(*testing.T, *Store) {}},
		{stSingle, initialize},
		{stPrepared, prepared},
		{stSwitched, switched},
		{"rolled-back", func(t *testing.T, s *Store) { switched(t, s); must(t, s.RollbackRotation()) }},
		{stRetired, func(t *testing.T, s *Store) { switched(t, s); must(t, s.RetireRotation(true)) }},
		{"replaced", func(t *testing.T, s *Store) { initialize(t, s); must(t, s.ReplaceCA()) }},
		{"replaced-from-switched", func(t *testing.T, s *Store) { switched(t, s); must(t, s.ReplaceCA()) }},
		// RetireRotation committed, then stopped before destroying the key.
		{"retired-key-left", func(t *testing.T, s *Store) {
			switched(t, s)
			key := keyBackup(t, s, 1)
			must(t, s.RetireRotation(true))
			restoreKey(t, s, 1, key)
		}},
		// ReplaceCA committed, then stopped before destroying the old key.
		{"replaced-key-left", func(t *testing.T, s *Store) {
			initialize(t, s)
			key := keyBackup(t, s, 1)
			must(t, s.ReplaceCA())
			restoreKey(t, s, 1, key)
		}},
		// As above, then a rotation prepared without an Ensure in between
		// (sproozi certificates rotate-ca runs PrepareRotation alone).
		{stReplacedKeyLeftPrepared, func(t *testing.T, s *Store) {
			initialize(t, s)
			key := keyBackup(t, s, 1)
			must(t, s.ReplaceCA())
			restoreKey(t, s, 1, key)
			must(t, s.PrepareRotation())
		}},
		{"serving-key-lost", func(t *testing.T, s *Store) {
			initialize(t, s)
			must(t, os.Remove(filepath.Join(s.Dir(), "ca-1.key")))
		}},
		{"prepared-new-key-lost", func(t *testing.T, s *Store) {
			prepared(t, s)
			must(t, os.Remove(filepath.Join(s.Dir(), keyCA2)))
		}},
		// PrepareRotation wrote the new CA's files, then stopped before
		// committing state.
		{"uncommitted-generation", func(t *testing.T, s *Store) {
			initialize(t, s)
			ca, err := NewCA("Sproozi local inspection CA 2", outcomeStart, DefaultPolicy.CAValidity)
			must(t, err)
			must(t, s.writePair(caName(2), ca))
		}},
		// issue committed a new leaf, then stopped before removing the old one.
		{"superseded-leaf-left", func(t *testing.T, s *Store) {
			initialize(t, s)
			st, err := s.State()
			must(t, err)
			pair, err := s.readPair(st.Leaf)
			must(t, err)
			must(t, s.Renew())
			must(t, s.writePair(st.Leaf, pair))
		}},
		// The state file is gone; the authority files remain.
		{stStateLost, func(t *testing.T, s *Store) {
			initialize(t, s)
			must(t, os.Remove(filepath.Join(s.Dir(), stateFile)))
		}},
	}
}

// outcomeEvent is one call a store's owner makes.
type outcomeEvent struct {
	name string
	run  func(s *Store) error
}

func outcomeEvents() []outcomeEvent {
	return []outcomeEvent{
		{"load", func(s *Store) error { _, err := s.State(); return err }},
		{"serving", func(s *Store) error { _, _, _, err := s.Serving(); return err }},
		{evNameInitialize, func(s *Store) error { return s.Initialize(testNames) }},
		{"ensure", func(s *Store) error { _, err := s.Ensure(testNames); return err }},
		{"renew", func(s *Store) error { return s.Renew() }},
		{"prepare", func(s *Store) error { return s.PrepareRotation() }},
		{"switch-undistributed", func(s *Store) error { return s.SwitchRotation(false) }},
		{evNameSwitch, func(s *Store) error { return s.SwitchRotation(true) }},
		{"rollback", func(s *Store) error { return s.RollbackRotation() }},
		{"retire-undrained", func(s *Store) error { return s.RetireRotation(false) }},
		{"retire", func(s *Store) error { return s.RetireRotation(true) }},
		{evNameReplace, func(s *Store) error { return s.ReplaceCA() }},
	}
}

// corruption edits a valid store into one a crash, a truncated copy or a
// hand edit could leave.
type corruption struct {
	name  string
	from  string
	apply func(t *testing.T, dir string)
}

func editState(edit func(st *State)) func(t *testing.T, dir string) {
	return func(t *testing.T, dir string) {
		t.Helper()
		path := filepath.Join(dir, stateFile)
		data, err := os.ReadFile(path)
		must(t, err)
		var st State
		must(t, json.Unmarshal(data, &st))
		edit(&st)
		data, err = json.MarshalIndent(st, "", "  ")
		must(t, err)
		must(t, os.WriteFile(path, append(data, '\n'), 0o600))
	}
}

func writeFile(name string, data func(t *testing.T, dir string) []byte) func(t *testing.T, dir string) {
	return func(t *testing.T, dir string) {
		t.Helper()
		must(t, os.WriteFile(filepath.Join(dir, name), data(t, dir), 0o600))
	}
}

func removeFile(name string) func(t *testing.T, dir string) {
	return func(t *testing.T, dir string) {
		t.Helper()
		must(t, os.Remove(filepath.Join(dir, name)))
	}
}

func newCAFile(part func(Pair) []byte) func(t *testing.T, dir string) []byte {
	return func(t *testing.T, _ string) []byte {
		ca, err := NewCA("hand-made", outcomeStart, DefaultPolicy.CAValidity)
		must(t, err)
		return part(ca)
	}
}

func corruptions() []corruption {
	return []corruption{
		{"truncated", stSingle, func(t *testing.T, dir string) {
			path := filepath.Join(dir, stateFile)
			data, err := os.ReadFile(path)
			must(t, err)
			must(t, os.WriteFile(path, data[:len(data)/2], 0o600))
		}},
		{"empty-file", stSingle, writeFile(stateFile, func(*testing.T, string) []byte { return nil })},
		{"unknown-field", stSingle, writeFile(stateFile, func(t *testing.T, dir string) []byte {
			data, err := os.ReadFile(filepath.Join(dir, stateFile))
			must(t, err)
			return bytes.Replace(data, []byte(`"version": 1,`), []byte(`"version": 1, "extra": true,`), 1)
		})},
		{corruptVersion2, stSingle, editState(func(st *State) { st.Version = 2 })},
		{"no-generations", stSingle, editState(func(st *State) { st.Generations = nil })},
		{"trusted-empty", stSingle, editState(func(st *State) { st.Trusted = nil })},
		{"serving-unknown", stSingle, editState(func(st *State) { st.Serving = 9 })},
		{"serving-retired", stSingle, editState(func(st *State) { st.Generations[0].Retired = true })},
		{"trusted-unknown", stSingle, editState(func(st *State) { st.Trusted = []int{1, 9} })},
		{"trusted-duplicate", stSingle, editState(func(st *State) { st.Trusted = []int{1, 1} })},
		{"trusted-retired", stRetired, editState(func(st *State) { st.Trusted = []int{1, 2} })},
		{"serving-not-trusted", stPrepared, editState(func(st *State) { st.Trusted = []int{2} })},
		{"generation-duplicate", stPrepared, editState(func(st *State) { st.Generations[1].ID = 1 })},
		{"generation-zero", stSingle, editState(func(st *State) {
			st.Generations[0].ID, st.Serving, st.Trusted = 0, 0, []int{0}
		})},
		{"generation-gap", stPrepared, editState(func(st *State) {
			st.Generations[1].ID, st.Trusted, st.Rotation.To = 3, []int{1, 3}, 3
		})},
		{"generation-not-retired-untrusted", stPrepared, editState(func(st *State) {
			st.Trusted, st.Rotation = []int{1}, nil
		})},
		{"fingerprint-empty", stSingle, editState(func(st *State) { st.Generations[0].Fingerprint = "" })},
		{"fingerprint-other", stSingle, editState(func(st *State) {
			st.Generations[0].Fingerprint = strings.Repeat("ab", 32)
		})},
		{"not-after-zero", stSingle, editState(func(st *State) { st.Generations[0].NotAfter = time.Time{} })},
		{"stage-unknown", stPrepared, editState(func(st *State) { st.Rotation.Stage = "bogus" })},
		{"stage-empty", stPrepared, editState(func(st *State) { st.Rotation.Stage = "" })},
		{"rotation-same-generation", stPrepared, editState(func(st *State) { st.Rotation.To = 1 })},
		{"rotation-unknown-generation", stPrepared, editState(func(st *State) { st.Rotation.To = 9 })},
		{"rotation-reversed", stPrepared, editState(func(st *State) { st.Rotation.From, st.Rotation.To = 2, 1 })},
		{"rotation-prepared-at-zero", stPrepared, editState(func(st *State) { st.Rotation.PreparedAt = time.Time{} })},
		{"prepared-serving-new", stPrepared, editState(func(st *State) { st.Serving = 2 })},
		{"switched-serving-old", stSwitched, editState(func(st *State) { st.Serving = 1 })},
		{"switched-stage-prepared", stSwitched, editState(func(st *State) { st.Rotation.Stage = StagePrepared })},
		{"prepared-trusted-reversed", stPrepared, editState(func(st *State) { st.Trusted = []int{2, 1} })},
		{"prepared-trusted-one", stPrepared, editState(func(st *State) { st.Trusted = []int{1} })},
		{"rotation-dropped", stPrepared, editState(func(st *State) { st.Rotation = nil })},
		{"rotation-after-retire", stRetired, editState(func(st *State) {
			st.Rotation = &Rotation{Stage: StageSwitched, From: 1, To: 2, PreparedAt: outcomeStart}
		})},
		{"retired-unmarked", stRetired, editState(func(st *State) { st.Generations[0].Retired = false })},
		{"leaf-outside", stSingle, editState(func(st *State) { st.Leaf = "../victim" })},
		{"leaf-unnamed", stSingle, editState(func(st *State) { st.Leaf = "" })},
		{"leaf-missing", stSingle, func(t *testing.T, dir string) {
			data, err := os.ReadFile(filepath.Join(dir, stateFile))
			must(t, err)
			var st State
			must(t, json.Unmarshal(data, &st))
			removeFile(st.Leaf+".pem")(t, dir)
			removeFile(st.Leaf+".key")(t, dir)
		}},
		{"ca-cert-missing", stSingle, removeFile("ca-1.pem")},
		{"new-ca-cert-missing", stPrepared, removeFile("ca-2.pem")},
		{"ca-cert-other", stSingle, writeFile("ca-1.pem", newCAFile(func(p Pair) []byte { return p.Certificate }))},
		{"ca-pair-other", stSingle, func(t *testing.T, dir string) {
			ca, err := NewCA("hand-made", outcomeStart, DefaultPolicy.CAValidity)
			must(t, err)
			must(t, os.WriteFile(filepath.Join(dir, "ca-1.pem"), ca.Certificate, 0o600))
			must(t, os.WriteFile(filepath.Join(dir, "ca-1.key"), ca.Key, 0o600))
		}},
		{"ca-key-other", stSingle, writeFile("ca-1.key", newCAFile(func(p Pair) []byte { return p.Key }))},
		{"new-ca-key-other", stPrepared, writeFile(keyCA2, newCAFile(func(p Pair) []byte { return p.Key }))},
		{"ca-key-garbage", stSingle, writeFile("ca-1.key", func(*testing.T, string) []byte { return []byte("not a key\n") })},
		{"retired-cert-missing", stRetired, removeFile("ca-1.pem")},
		{"trust-file-missing", stSingle, removeFile(trustFile)},
	}
}

var (
	leafName  = regexp.MustCompile(`^leaf-[0-9a-f]{16}$`)
	leafFile  = regexp.MustCompile(`^leaf-[0-9a-f]{16}\.(pem|key)$`)
	caFileRE  = regexp.MustCompile(`^ca-(\d+)\.(pem|key)$`)
	timestamp = regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ`)
	leafHex   = regexp.MustCompile(`leaf-[0-9a-f]{16}`)
)

// describeStore summarises a store by what callers can observe: the stage,
// the serving, trusted and retired generations, the CA files, how many leaf
// files there are, which CA issued the named leaf, which CAs the worker
// bundle holds, and whether anything outside the store was removed.
func describeStore(root, leafBefore string) string {
	dir := filepath.Join(root, "pki")
	var parts []string
	data, err := os.ReadFile(filepath.Join(dir, stateFile))
	var st State
	switch {
	case os.IsNotExist(err):
		parts = append(parts, "state=absent")
	case err != nil || json.Unmarshal(data, &st) != nil:
		parts = append(parts, "state=unreadable")
	default:
		stage := "none"
		if st.Rotation != nil {
			stage = fmt.Sprintf("%s:%d>%d", st.Rotation.Stage, st.Rotation.From, st.Rotation.To)
		}
		var retired []int
		for _, g := range st.Generations {
			if g.Retired {
				retired = append(retired, g.ID)
			}
		}
		parts = append(parts, "stage="+stage, fmt.Sprintf("serving=%d trusted=%v retired=%v", st.Serving, st.Trusted, retired))
	}
	entries, _ := os.ReadDir(dir)
	var ca []string
	leaves := 0
	cas := map[int]*x509.Certificate{}
	for _, entry := range entries {
		name := entry.Name()
		if leafFile.MatchString(name) {
			leaves++
		}
		if m := caFileRE.FindStringSubmatch(name); m != nil {
			ca = append(ca, name)
			if m[2] == "pem" {
				var id int
				_, _ = fmt.Sscan(m[1], &id)
				if raw, err := os.ReadFile(filepath.Join(dir, name)); err == nil {
					cas[id], _ = ParseCertificate(raw)
				}
			}
		}
	}
	slices.Sort(ca)
	parts = append(parts, fmt.Sprintf("ca=%v leafFiles=%d", ca, leaves))
	switch {
	case st.Leaf == "":
		parts = append(parts, "leaf=none")
	case !leafName.MatchString(st.Leaf):
		parts = append(parts, "leaf=invalid")
	default:
		issuer := "?"
		if raw, err := os.ReadFile(filepath.Join(dir, st.Leaf+".pem")); err == nil {
			if leaf, err := ParseCertificate(raw); err == nil {
				for id, c := range cas {
					if c != nil && leaf.CheckSignatureFrom(c) == nil {
						issuer = fmt.Sprint(id)
					}
				}
			}
		} else {
			issuer = "missing"
		}
		changed := "kept"
		if st.Leaf != leafBefore {
			changed = "new"
		}
		parts = append(parts, fmt.Sprintf("leaf=%s issuer=%s", changed, issuer))
	}
	if raw, err := os.ReadFile(filepath.Join(dir, trustFile)); err == nil {
		var in []int
		for rest := raw; ; {
			var block *pem.Block
			block, rest = pem.Decode(rest)
			if block == nil {
				break
			}
			id := 0
			for gid, c := range cas {
				if c != nil && bytes.Equal(c.Raw, block.Bytes) {
					id = gid
				}
			}
			in = append(in, id)
		}
		parts = append(parts, fmt.Sprintf("bundle=%v", in))
	} else {
		parts = append(parts, "bundle=absent")
	}
	for _, name := range victimFiles {
		if _, err := os.Stat(filepath.Join(root, name)); os.IsNotExist(err) {
			parts = append(parts, "outside:"+name+"=removed")
		}
	}
	return strings.Join(parts, " ")
}

func leafOf(root string) string {
	data, err := os.ReadFile(filepath.Join(root, "pki", stateFile))
	if err != nil {
		return ""
	}
	var st State
	if json.Unmarshal(data, &st) != nil {
		return ""
	}
	return st.Leaf
}

func outcomeError(err error, root string) string {
	if err == nil {
		return "ok"
	}
	msg := strings.ReplaceAll(err.Error(), root, "$ROOT")
	msg = timestamp.ReplaceAllString(msg, "$$TIME")
	msg = leafHex.ReplaceAllString(msg, "leaf-X")
	return "error: " + strings.ReplaceAll(msg, "\n", " / ")
}

// recordOutcome runs ev on a copy of the store in base and describes the
// result.
func recordOutcome(t *testing.T, base string, ev outcomeEvent) string {
	t.Helper()
	root := copyStore(t, filepath.Join(base, "pki"))
	s := outcomeStore(t, root)
	before := leafOf(root)
	err := ev.run(s)
	return outcomeError(err, root) + " | " + describeStore(root, before)
}

func outcomeLines(t *testing.T) []string {
	t.Helper()
	bases := buildStates(t)
	lines := make([]string, 0, 1024)
	for _, state := range outcomeStates() {
		root := bases[state.name]
		lines = append(lines, fmt.Sprintf("state %s | start | %s", state.name, describeStore(root, leafOf(root))))
		for _, ev := range outcomeEvents() {
			lines = append(lines, fmt.Sprintf("state %s | %s -> %s", state.name, ev.name, recordOutcome(t, root, ev)))
		}
	}
	for _, c := range corruptions() {
		root := copyStore(t, filepath.Join(bases[c.from], "pki"))
		c.apply(t, filepath.Join(root, "pki"))
		for _, ev := range outcomeEvents() {
			lines = append(lines, fmt.Sprintf("corrupt %s (from %s) | %s -> %s", c.name, c.from, ev.name, recordOutcome(t, root, ev)))
		}
	}
	return lines
}

// Every store operation, from every state the store writes or an
// interruption leaves, and from every corrupted record in corruptions(), has
// the outcome recorded in testdata/store-outcomes.txt. The file was first
// generated from the store before its rotation record adopted ADR 0058's
// pattern; each later difference is a deliberate change in its own commit.
func TestStoreOutcomesMatchTheRecordedTable(t *testing.T) {
	t.Parallel() // one of the largest tables; see sharedStates
	lines := outcomeLines(t)
	got := strings.Join(lines, "\n") + "\n"
	if *updateOutcomes {
		must(t, os.MkdirAll(filepath.Dir(outcomesFile), 0o755))
		must(t, os.WriteFile(outcomesFile, []byte(got), 0o600))
		return
	}
	want, err := os.ReadFile(outcomesFile)
	must(t, err)
	wantLines := strings.Split(strings.TrimSuffix(string(want), "\n"), "\n")
	if len(wantLines) != len(lines) {
		t.Fatalf("enumerated %d outcomes, recorded %d", len(lines), len(wantLines))
	}
	for i := range lines {
		if lines[i] != wantLines[i] {
			t.Errorf("outcome changed:\n got %s\nwant %s", lines[i], wantLines[i])
		}
	}
}
