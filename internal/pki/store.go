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
var ErrNotInitialized = errors.New("pki: no local certificate authority; run sproozi init --local")

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

// State reads the committed state.
func (s *Store) State() (State, error) {
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
	if st.Version != 1 || st.generation(st.Serving) == nil || len(st.Trusted) == 0 {
		return st, errors.New("pki: corrupt state")
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

// Ensure creates the first CA when absent and issues a new leaf when the
// current one is missing, invalid, due for renewal, issued by a CA other than
// the serving one, or does not cover names. It never replaces an existing CA.
// It reports whether the serving leaf changed.
func (s *Store) Ensure(names Names) (bool, error) {
	names, err := names.normalized()
	if err != nil {
		return false, err
	}
	changed := false
	err = s.locked(func() error {
		now := s.now()
		st, err := s.State()
		if errors.Is(err, ErrNotInitialized) {
			if err = s.createGeneration(1, now); err != nil {
				return err
			}
			st = State{Version: 1, Serving: 1, Trusted: []int{1}}
			if st.Generations, err = s.generations(st, 1); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		same := slices.Equal(st.Names.DNS, names.DNS) && slices.Equal(st.Names.IPs, names.IPs)
		st.Names = names
		if leaf, err := s.loadLeaf(st); same && err == nil && s.leafCurrent(st, leaf.Leaf, now) {
			return s.publishTrust(st)
		}
		changed = true
		return s.issue(&st, now)
	})
	return changed, err
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
		if st.Rotation != nil {
			return fmt.Errorf("pki: rotation already %s", st.Rotation.Stage)
		}
		next := 0
		for _, g := range st.Generations {
			next = max(next, g.ID)
		}
		next++
		now := s.now()
		if err = s.createGeneration(next, now); err != nil {
			return err
		}
		st.Trusted = []int{st.Serving, next}
		st.Rotation = &Rotation{Stage: StagePrepared, From: st.Serving, To: next, PreparedAt: now.UTC()}
		if st.Generations, err = s.generations(st, next); err != nil {
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
		if st.Rotation == nil || st.Rotation.Stage != StagePrepared {
			return errors.New("pki: no prepared rotation to switch")
		}
		if !distributed {
			return errors.New("pki: combined trust is not yet distributed to every current consumer")
		}
		st.Serving = st.Rotation.To
		st.Rotation.Stage = StageSwitched
		return s.issue(&st, s.now())
	})
}

// RollbackRotation returns to serving the previous CA while both remain trusted.
func (s *Store) RollbackRotation() error {
	return s.locked(func() error {
		st, err := s.State()
		if err != nil {
			return err
		}
		if st.Rotation == nil || st.Rotation.Stage != StageSwitched {
			return errors.New("pki: no switched rotation to roll back")
		}
		st.Serving = st.Rotation.From
		st.Rotation.Stage = StagePrepared
		return s.issue(&st, s.now())
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
		if st.Rotation == nil || st.Rotation.Stage != StageSwitched {
			return errors.New("pki: rotation must switch before retirement")
		}
		if !drained {
			return errors.New("pki: a consumer may still require the retiring CA")
		}
		old := st.Rotation.From
		st.Trusted = []int{st.Rotation.To}
		st.Rotation = nil
		if g := st.generation(old); g != nil {
			g.Retired = true
		}
		if err = s.commit(st); err != nil {
			return err
		}
		return s.destroyKey(old)
	})
}

// ReplaceCA is the emergency path for a lost or compromised CA. It trusts only
// a new generation, issues a new leaf and destroys every older private key.
// Existing workers lose trust and must be cancelled or drained by the caller.
func (s *Store) ReplaceCA() error {
	return s.locked(func() error {
		st, err := s.State()
		if err != nil {
			return err
		}
		next := 0
		for _, g := range st.Generations {
			next = max(next, g.ID)
		}
		next++
		now := s.now()
		if err = s.createGeneration(next, now); err != nil {
			return err
		}
		for i := range st.Generations {
			st.Generations[i].Retired = true
		}
		st.Serving, st.Trusted, st.Rotation = next, []int{next}, nil
		if st.Generations, err = s.generations(st, next); err != nil {
			return err
		}
		if err = s.issue(&st, now); err != nil {
			return err
		}
		for _, g := range st.Generations {
			if g.ID != next {
				if err = s.destroyKey(g.ID); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// RotationDue reports whether routine rotation of the serving CA should begin.
func (s *Store) RotationDue(st State) bool {
	g := st.generation(st.Serving)
	return st.Rotation == nil && g != nil && !s.now().Before(g.NotAfter.Add(-s.policy.RotateBefore))
}

func (s *Store) issue(st *State, now time.Time) error {
	ca, err := s.readPair(caName(st.Serving))
	if err != nil {
		return fmt.Errorf("pki: serving CA key unavailable; recover it or replace the CA: %w", err)
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
	previous := st.Leaf
	st.Leaf = name
	if err = s.commit(*st); err != nil {
		return err
	}
	if previous != "" && previous != name {
		_ = os.Remove(filepath.Join(s.dir, previous+".key"))
		_ = os.Remove(filepath.Join(s.dir, previous+".pem"))
	}
	return nil
}

func (s *Store) commit(st State) error {
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err = privatefs.Replace(s.dir, stateFile, append(data, '\n')); err != nil {
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
	return privatefs.Replace(s.dir, trustFile, trust)
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

func (s *Store) createGeneration(id int, now time.Time) error {
	if _, err := os.Lstat(filepath.Join(s.dir, caName(id)+".pem")); err == nil {
		// A crash after writing an uncommitted generation leaves its files;
		// the next attempt replaces them because no state names them.
		_ = os.Remove(filepath.Join(s.dir, caName(id)+".pem"))
		_ = os.Remove(filepath.Join(s.dir, caName(id)+".key"))
	}
	ca, err := NewCA(fmt.Sprintf("Sproozi local inspection CA %d", id), now, s.policy.CAValidity)
	if err != nil {
		return err
	}
	return s.writePair(caName(id), ca)
}

func (s *Store) generations(st State, added int) ([]Generation, error) {
	cert, err := s.caCertificate(added)
	if err != nil {
		return nil, err
	}
	out := slices.Clone(st.Generations)
	return append(out, Generation{ID: added, Fingerprint: Fingerprint(cert), NotAfter: cert.NotAfter}), nil
}

func (s *Store) destroyKey(id int) error {
	err := os.Remove(filepath.Join(s.dir, caName(id)+".key"))
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
	return errors.Join(privatefs.Replace(s.dir, name+".key", p.Key), privatefs.Replace(s.dir, name+".pem", p.Certificate))
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
