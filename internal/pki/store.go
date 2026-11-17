package pki

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/privatefs"
)

// Stage is the durable step of a routine CA rotation.
type Stage string

const (
	// StagePrepared trusts the old and new CA while the old CA still issues the
	// serving leaf. New consumers receive both certificates.
	StagePrepared Stage = "prepared"
	// StageSwitched serves a leaf from the new CA while both remain trusted.
	StageSwitched Stage = "switched"
)

// Generation is one CA. Retired generations keep their public certificate in
// history; their private key is destroyed at retirement.
type Generation struct {
	ID          int       `json:"id"`
	Fingerprint string    `json:"fingerprint"`
	NotAfter    time.Time `json:"notAfter"`
	Retired     bool      `json:"retired,omitempty"`
}

// Rotation records an in-progress routine rotation from one generation to another.
type Rotation struct {
	Stage      Stage     `json:"stage"`
	From       int       `json:"from"`
	To         int       `json:"to"`
	PreparedAt time.Time `json:"preparedAt"`
}

// State is the store's single atomic commit point. Key and certificate files
// are written before the state that names them.
type State struct {
	Version     int          `json:"version"`
	Names       Names        `json:"names"`
	Generations []Generation `json:"generations"`
	Serving     int          `json:"serving"`
	Trusted     []int        `json:"trusted"`
	Leaf        string       `json:"leaf,omitempty"`
	Rotation    *Rotation    `json:"rotation,omitempty"`
}

// Store is a private directory of CA generations, the serving leaf and the
// worker trust bundle. Every mutation holds the directory lock; readers use the
// committed state and never see a partially written leaf.
type Store struct {
	dir    string
	policy Policy
	now    func() time.Time
	// beforeWrite, when set, runs before every change to the directory's
	// authority, leaf, trust and state files. A test returns an error from it
	// to stop the store at one write, as a crash would. It is nil in
	// production.
	beforeWrite func(op, name string) error
}

const (
	stateFile   = "state.json"
	trustFile   = "trust.pem"
	lockFile    = "owner.lock"
	maxPEM      = 64 << 10
	maxState    = 64 << 10
	lockTimeout = 10 * time.Second
)

// ErrNotInitialized means the store has no committed CA.
var ErrNotInitialized = errors.New("pki: no local certificate authority; run sproozi init")

// Open validates or creates the private directory.
func Open(dir string, policy Policy, now func() time.Time) (*Store, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("pki: absolute directory required")
	}
	if err := policy.validate(); err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	if err := privatefs.EnsureDir(dir); err != nil {
		return nil, err
	}
	return &Store{dir: dir, policy: policy, now: now}, nil
}

// Dir is the store's private directory.
func (s *Store) Dir() string { return s.dir }

// Policy is the store's lifetime policy.
func (s *Store) Policy() Policy { return s.policy }

// TrustPath is the certificate-only worker bundle.
func (s *Store) TrustPath() string { return filepath.Join(s.dir, trustFile) }

// State reads the committed state. It refuses a record no phase describes,
// and one whose trusted generations' files are not the ones it names.
func (s *Store) State() (State, error) {
	st, err := s.load()
	if err != nil {
		return st, err
	}
	return st, s.checkFiles(st)
}

// load reads the committed record and refuses one no phase describes. It
// does not read the authority files: Initialize uses it.
func (s *Store) load() (State, error) {
	st, err := s.decode()
	if err != nil {
		return st, err
	}
	return st, check(st)
}

// decode reads the committed record without checking what it describes.
func (s *Store) decode() (State, error) {
	var st State
	data, err := privatefs.ReadFile(filepath.Join(s.dir, stateFile), maxState)
	if os.IsNotExist(err) {
		return st, ErrNotInitialized
	}
	if err != nil {
		return st, err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err = d.Decode(&st); err != nil {
		return st, fmt.Errorf("pki: corrupt state: %w", err)
	}
	return st, nil
}

func (st State) generation(id int) *Generation {
	for i := range st.Generations {
		if st.Generations[i].ID == id {
			return &st.Generations[i]
		}
	}
	return nil
}

// Serving loads the committed serving certificate and its trust bundle.
func (s *Store) Serving() (tls.Certificate, []byte, State, error) {
	st, err := s.State()
	if err != nil {
		return tls.Certificate{}, nil, st, err
	}
	certificate, err := s.loadLeaf(st)
	if err != nil {
		return tls.Certificate{}, nil, st, err
	}
	trust, err := privatefs.ReadFile(s.TrustPath(), maxPEM)
	if err != nil {
		return tls.Certificate{}, nil, st, err
	}
	return certificate, trust, st, nil
}

func (s *Store) loadLeaf(st State) (tls.Certificate, error) {
	if st.Leaf == "" {
		return tls.Certificate{}, errors.New("pki: no serving leaf")
	}
	pair, err := s.readPair(st.Leaf)
	if err != nil {
		return tls.Certificate{}, err
	}
	certificate, err := tls.X509KeyPair(pair.Certificate, pair.Key)
	if err != nil {
		return tls.Certificate{}, err
	}
	certificate.Leaf, err = x509.ParseCertificate(certificate.Certificate[0])
	return certificate, err
}

// ErrAuthorityMaterial means the store holds authority files but no
// committed state. It is never treated as a fresh store.
var ErrAuthorityMaterial = errors.New("pki: certificate authority files exist without committed state; " +
	"restore state.json from a backup, or replace the authority explicitly")

// Initialize creates the first CA and serving leaf in an empty store. It
// refuses when any authority or leaf file already exists, so a lost state file
// never silently becomes an unrelated authority.
func (s *Store) Initialize(names Names) error {
	names, err := names.normalized()
	if err != nil {
		return err
	}
	return s.locked(func() error {
		st, err := s.load()
		if err == nil {
			return st.admit(evInitialize)
		}
		if !errors.Is(err, ErrNotInitialized) {
			return err
		}
		if err = s.requireEmpty(); err != nil {
			return err
		}
		st = State{Names: names}
		now := s.now()
		first, err := s.createNext(st, now)
		if err != nil {
			return err
		}
		if err = st.apply(evInitialize, &first, now); err != nil {
			return err
		}
		return s.issue(&st, now)
	})
}

// requireEmpty refuses a store that holds any CA or leaf file.
func (s *Store) requireEmpty() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, "ca-") || strings.HasPrefix(name, "leaf-") || name == trustFile {
			return ErrAuthorityMaterial
		}
	}
	return nil
}

// Ensure issues a new leaf when the current one is missing, invalid, due for
// renewal, issued by a CA other than the serving one, or does not cover
// names. It never creates or replaces a CA; an uninitialized store fails.
// It then destroys any retired key or stray file an interrupted operation
// left behind. A removal that fails does not stop the leaf and trust work:
// Ensure completes it, then returns the failure marked ErrOutstanding, and
// the next Ensure tries again. It reports whether the serving leaf changed.
func (s *Store) Ensure(names Names) (bool, error) {
	names, err := names.normalized()
	if err != nil {
		return false, err
	}
	changed := false
	err = s.locked(func() error {
		now := s.now()
		st, err := s.State()
		if errors.Is(err, ErrNotInitialized) && s.requireEmpty() != nil {
			return ErrAuthorityMaterial
		}
		if err != nil {
			return err
		}
		same := slices.Equal(st.Names.DNS, names.DNS) && slices.Equal(st.Names.IPs, names.IPs)
		if !same {
			if err = namesFit(st, names); err != nil {
				return err
			}
		}
		st.Names = names
		if leaf, loadErr := s.loadLeaf(st); same && loadErr == nil && s.leafCurrent(st, leaf.Leaf, now) {
			err = s.publishTrust(st)
		} else {
			changed = true
			err = s.issue(&st, now)
		}
		cleanup := errors.Join(s.destroyRetired(st), s.checkKeys(st))
		if err != nil {
			return errors.Join(err, cleanup)
		}
		return outstanding(cleanup)
	})
	return changed, err
}

// ErrOutstanding marks an error Ensure returns only when it completed its
// leaf and trust work: something it found could not be cleaned up, such as
// a retired key it could not remove. The serving leaf and trust bundle are
// current and committed; the error must still be reported, and the next
// Ensure retries.
var ErrOutstanding = errors.New("pki: outstanding certificate store cleanup")

type outstandingError struct{ err error }

func (e outstandingError) Error() string   { return e.err.Error() }
func (e outstandingError) Unwrap() []error { return []error{ErrOutstanding, e.err} }

func outstanding(err error) error {
	if err == nil {
		return nil
	}
	return outstandingError{err}
}

func (s *Store) leafCurrent(st State, leaf *x509.Certificate, now time.Time) bool {
	ca, err := s.caCertificate(st.Serving)
	if err != nil || leaf.CheckSignatureFrom(ca) != nil {
		return false
	}
	trust, err := s.trustBundle(st)
	if err != nil || VerifyServing(leaf, trust, st.Names, now) != nil {
		return false
	}
	return now.Before(s.policy.RenewAt(leaf))
}

// Renew issues a new leaf from the serving CA regardless of remaining lifetime.
func (s *Store) Renew() error {
	return s.locked(func() error {
		st, err := s.State()
		if err != nil {
			return err
		}
		return s.issue(&st, s.now())
	})
}

// PrepareRotation creates the next CA and trusts both generations. The old CA
// keeps issuing the serving leaf until SwitchRotation.
func (s *Store) PrepareRotation() error {
	return s.locked(func() error {
		st, err := s.State()
		if err != nil {
			return err
		}
		if err = st.admit(evPrepare); err != nil {
			return err
		}
		if err = startsWithin(st, string(evPrepare)); err != nil {
			return err
		}
		now := s.now()
		next, err := s.createNext(st, now)
		if err != nil {
			return err
		}
		if err = st.apply(evPrepare, &next, now); err != nil {
			return err
		}
		return s.commit(st)
	})
}

// SwitchRotation serves a leaf from the new CA. distributed must state that
// every current trust consumer has received the combined bundle or stopped.
func (s *Store) SwitchRotation(distributed bool) error {
	return s.locked(func() error {
		st, err := s.State()
		if err != nil {
			return err
		}
		if err = st.admit(evSwitch); err != nil {
			return err
		}
		if !distributed {
			return errors.New("pki: combined trust is not yet distributed to every current consumer")
		}
		now := s.now()
		if err = st.apply(evSwitch, nil, now); err != nil {
			return err
		}
		return s.issue(&st, now)
	})
}

// RollbackRotation returns to serving the previous CA while both remain trusted.
func (s *Store) RollbackRotation() error {
	return s.locked(func() error {
		st, err := s.State()
		if err != nil {
			return err
		}
		now := s.now()
		if err = st.apply(evRollback, nil, now); err != nil {
			return err
		}
		return s.issue(&st, now)
	})
}

// RetireRotation removes the old CA from trust and destroys its private key.
// drained must state that no consumer still requires the old CA.
func (s *Store) RetireRotation(drained bool) error {
	return s.locked(func() error {
		st, err := s.State()
		if err != nil {
			return err
		}
		if err = st.admit(evRetire); err != nil {
			return err
		}
		if !drained {
			return errors.New("pki: a consumer may still require the retiring CA")
		}
		// Retiring destroys the outgoing key; first prove the serving key,
		// which will then be the only one, works for its certificate.
		if _, err = s.servingPair(st); err != nil {
			return fmt.Errorf("pki: refusing to retire generation %d: %w", st.Rotation.From, err)
		}
		if err = st.apply(evRetire, nil, s.now()); err != nil {
			return err
		}
		if err = s.commit(st); err != nil {
			return err
		}
		return s.destroyRetired(st)
	})
}

// ReplaceCA is the emergency path for a lost or compromised CA. It trusts only
// a new generation, issues a new leaf and destroys every older private key.
// Existing workers lose trust and must be cancelled or drained by the caller.
// It is also the way out of a record no phase describes, or whose
// certificates are not the ones it names: it reads the record without that
// check, and builds on such a record as replaceable does. It refuses only a
// record it cannot read, or one of another version.
func (s *Store) ReplaceCA() error {
	return s.locked(func() error {
		st, err := s.decode()
		if err != nil {
			return err
		}
		if st.Version != 1 {
			return check(st)
		}
		if check(st) != nil || s.checkFiles(st) != nil {
			if st, err = s.replaceable(st); err != nil {
				return err
			}
		}
		if err = st.admit(evReplace); err != nil {
			return err
		}
		if err = startsWithin(st, string(evReplace)); err != nil {
			return err
		}
		now := s.now()
		next, err := s.createNext(st, now)
		if err != nil {
			return err
		}
		if err = st.apply(evReplace, &next, now); err != nil {
			return err
		}
		if err = s.issue(&st, now); err != nil {
			return err
		}
		return s.destroyRetired(st)
	})
}

// RotationDue reports whether routine rotation of the serving CA should begin.
func (s *Store) RotationDue(st State) bool {
	g := st.generation(st.Serving)
	return st.Rotation == nil && g != nil && !s.now().Before(g.NotAfter.Add(-s.policy.RotateBefore))
}

// servingPair reads the serving generation's certificate and key and proves
// the key matches the certificate, naming the generation, the file and the
// cause otherwise.
func (s *Store) servingPair(st State) (Pair, error) {
	name := caName(st.Serving)
	pair, err := s.readPair(name)
	if err == nil {
		_, _, err = Parse(pair)
	}
	if err != nil {
		return Pair{}, fmt.Errorf("pki: serving CA key unavailable; recover it or replace the CA: generation %d key %s.key: %w",
			st.Serving, name, err)
	}
	return pair, nil
}

func (s *Store) issue(st *State, now time.Time) error {
	ca, err := s.servingPair(*st)
	if err != nil {
		return err
	}
	leaf, err := IssueLeaf(ca, st.Names, now, s.policy.LeafValidity)
	if err != nil {
		return err
	}
	cert, _, err := Parse(leaf)
	if err != nil {
		return err
	}
	name := "leaf-" + Fingerprint(cert)[:16]
	if err = s.writePair(name, leaf); err != nil {
		return err
	}
	st.Leaf = name
	if err = s.commit(*st); err != nil {
		return err
	}
	// The leaf is committed; a removal that fails here is retried by the
	// next Ensure.
	_ = s.destroyRetired(*st)
	return nil
}

func (s *Store) commit(st State) error {
	if err := errors.Join(check(st), s.checkFiles(st)); err != nil {
		return fmt.Errorf("refusing to save: %w", err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if len(data)+1 > maxState {
		return fmt.Errorf("refusing to save: the record would be %d bytes, more than the %d bytes load reads",
			len(data)+1, maxState)
	}
	if err = s.replace(stateFile, append(data, '\n')); err != nil {
		return err
	}
	return s.publishTrust(st)
}

// publishTrust rewrites the derived worker bundle from committed state.
func (s *Store) publishTrust(st State) error {
	trust, err := s.trustBundle(st)
	if err != nil {
		return err
	}
	current, err := privatefs.ReadFile(s.TrustPath(), maxPEM)
	if err == nil && bytes.Equal(current, trust) {
		return nil
	}
	return s.replace(trustFile, trust)
}

func (s *Store) trustBundle(st State) ([]byte, error) {
	var bundle []byte
	for _, id := range st.Trusted {
		data, err := privatefs.ReadFile(filepath.Join(s.dir, caName(id)+".pem"), maxPEM)
		if err != nil {
			return nil, err
		}
		bundle = append(bundle, data...)
	}
	return bundle, nil
}

func (s *Store) caCertificate(id int) (*x509.Certificate, error) {
	data, err := privatefs.ReadFile(filepath.Join(s.dir, caName(id)+".pem"), maxPEM)
	if err != nil {
		return nil, err
	}
	return ParseCertificate(data)
}

// createGeneration writes a new CA's files. Callers hold committed state that
// does not name id, so existing files for id are an uncommitted earlier
// attempt and may be replaced.
func (s *Store) createGeneration(id int, now time.Time) error {
	if _, err := os.Lstat(filepath.Join(s.dir, caName(id)+".pem")); err == nil {
		_ = s.remove(caName(id) + ".pem")
		_ = s.remove(caName(id) + ".key")
	}
	ca, err := NewCA(fmt.Sprintf("Sproozi local inspection CA %d", id), now, s.policy.CAValidity)
	if err != nil {
		return err
	}
	return s.writePair(caName(id), ca)
}

// createNext writes the files of the generation after every recorded one
// and returns its record entry.
func (s *Store) createNext(st State, now time.Time) (Generation, error) {
	id := len(st.Generations) + 1
	if err := s.createGeneration(id, now); err != nil {
		return Generation{}, err
	}
	cert, err := s.caCertificate(id)
	if err != nil {
		return Generation{}, err
	}
	return Generation{ID: id, Fingerprint: Fingerprint(cert), NotAfter: cert.NotAfter}, nil
}

// destroyRetired removes the private key of every retired generation; the
// certificate and key of every generation numbered after the record's last,
// which an interrupted prepare or replace wrote and never committed; and
// every leaf file other than the leaf the record names: superseded leaves,
// and an uncommitted one an interrupted issue wrote. It is idempotent: Ensure
// runs it on every reconcile of the certificate controller and at every start
// and maintenance pass of the local service, retire and replace run it after
// their commit, and every issue after its own, so a destruction that a crash
// or a failed removal left undone always finishes. It never removes the key
// of a trusted generation, which every phase still needs, nor the named
// leaf. It removes only names the store writes: ca-<N>.key for a recorded N,
// ca-<N>.pem and ca-<N>.key matching caFilePattern, and leaf-<16 hex>.pem
// and .key. It runs under the store's lock, which every operation that
// writes a generation or a leaf holds from its first file to its commit, so
// it never sees another operation's files before their commit.
func (s *Store) destroyRetired(st State) error {
	var errs []error
	for _, g := range st.Generations {
		if g.Retired {
			errs = append(errs, s.destroyKey(g.ID))
		}
	}
	entries, err := os.ReadDir(s.dir)
	errs = append(errs, err)
	for _, entry := range entries {
		name := entry.Name()
		if caFileGeneration(name) > len(st.Generations) {
			if err = s.remove(name); !os.IsNotExist(err) {
				errs = append(errs, err)
			}
			continue
		}
		base, ok := strings.CutSuffix(name, filepath.Ext(name))
		if ok && leafNamePattern.MatchString(base) && (filepath.Ext(name) == ".key" || filepath.Ext(name) == ".pem") &&
			base != st.Leaf {
			if err = s.remove(name); !os.IsNotExist(err) {
				errs = append(errs, err)
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("pki: destroy retired keys: %w", err)
	}
	return nil
}

func (s *Store) destroyKey(id int) error {
	if _, err := os.Lstat(filepath.Join(s.dir, caName(id)+".key")); os.IsNotExist(err) {
		return nil
	}
	err := s.remove(caName(id) + ".key")
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

func (s *Store) readPair(name string) (Pair, error) {
	cert, err := privatefs.ReadFile(filepath.Join(s.dir, name+".pem"), maxPEM)
	if err != nil {
		return Pair{}, err
	}
	key, err := privatefs.ReadFile(filepath.Join(s.dir, name+".key"), maxPEM)
	if err != nil {
		return Pair{}, err
	}
	return Pair{Certificate: cert, Key: key}, nil
}

func (s *Store) writePair(name string, p Pair) error {
	return errors.Join(s.replace(name+".key", p.Key), s.replace(name+".pem", p.Certificate))
}

// replace writes one file in the store's directory.
func (s *Store) replace(name string, data []byte) error {
	if s.beforeWrite != nil {
		if err := s.beforeWrite("write", name); err != nil {
			return err
		}
	}
	return privatefs.Replace(s.dir, name, data)
}

// remove deletes one file in the store's directory.
func (s *Store) remove(name string) error {
	if s.beforeWrite != nil {
		if err := s.beforeWrite("remove", name); err != nil {
			return err
		}
	}
	return os.Remove(filepath.Join(s.dir, name))
}

func (s *Store) locked(fn func() error) error {
	deadline := time.Now().Add(lockTimeout)
	for {
		lock, err := privatefs.Lock(filepath.Join(s.dir, lockFile))
		if err == nil {
			defer func() { _ = privatefs.UnlockClose(lock) }()
			return fn()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("pki: certificate store busy: %w", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func caName(id int) string { return fmt.Sprintf("ca-%d", id) }

// Consumer is a trust recipient that may still hold an earlier bundle, such as
// a running worker allocation. Reference is opaque caller evidence used to
// confirm the consumer independently stopped.
type Consumer struct {
	ID          string          `json:"id"`
	Generations []int           `json:"generations"`
	Reference   json.RawMessage `json:"reference,omitempty"`
}

const consumersDir = "consumers"

// TrustFor returns the current worker bundle and durably records that consumer
// id received it, before the bundle can leave the trusted service.
func (s *Store) TrustFor(id string, reference json.RawMessage) ([]byte, error) {
	if !validConsumer(id) {
		return nil, errors.New("pki: invalid trust consumer")
	}
	var trust []byte
	err := s.locked(func() error {
		st, err := s.State()
		if err != nil {
			return err
		}
		if trust, err = s.trustBundle(st); err != nil {
			return err
		}
		dir := filepath.Join(s.dir, consumersDir)
		if err = privatefs.EnsureDir(dir); err != nil {
			return err
		}
		data, err := json.Marshal(Consumer{ID: id, Generations: slices.Clone(st.Trusted), Reference: reference})
		if err != nil {
			return err
		}
		return privatefs.Replace(dir, id+".json", data)
	})
	return trust, err
}

// Release forgets a consumer after the caller independently confirmed it stopped.
func (s *Store) Release(id string) error {
	if !validConsumer(id) {
		return errors.New("pki: invalid trust consumer")
	}
	err := os.Remove(filepath.Join(s.dir, consumersDir, id+".json"))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Consumers lists recorded trust consumers.
func (s *Store) Consumers() ([]Consumer, error) {
	dir := filepath.Join(s.dir, consumersDir)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Consumer
	for _, entry := range entries {
		name := entry.Name()
		if filepath.Ext(name) != ".json" || !validConsumer(name[:len(name)-5]) {
			continue
		}
		data, err := privatefs.ReadFile(filepath.Join(dir, name), maxState)
		if err != nil {
			return nil, err
		}
		var c Consumer
		if err = json.Unmarshal(data, &c); err != nil || c.ID+".json" != name {
			return nil, fmt.Errorf("pki: corrupt trust consumer %s", name)
		}
		out = append(out, c)
	}
	return out, nil
}

// Lacking lists consumers whose recorded trust does not include generation.
func (s *Store) Lacking(generation int) ([]Consumer, error) {
	consumers, err := s.Consumers()
	if err != nil {
		return nil, err
	}
	var out []Consumer
	for _, c := range consumers {
		if !slices.Contains(c.Generations, generation) {
			out = append(out, c)
		}
	}
	return out, nil
}

func validConsumer(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
			return false
		}
	}
	return true
}
