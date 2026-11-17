package pki

import (
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"

	"github.com/andrewmccall/sproozi/internal/privatefs"
)

// Phase is where the store's rotation record is in its lifecycle, as ADR
// 0058 asks of a lifecycle record. The record stores it as its rotation's
// stage, with no rotation meaning PhaseSingle; describe derives it from which
// generations are trusted and which one serves, and every load and save
// requires the two to agree.
type Phase string

const (
	// PhaseSingle trusts one generation, which also serves. Every other
	// generation is retired.
	PhaseSingle Phase = "single"
	// PhasePrepared trusts the serving generation and the newest one.
	PhasePrepared Phase = Phase(StagePrepared)
	// PhaseSwitched trusts the previous generation and the newest one, which
	// serves.
	PhaseSwitched Phase = Phase(StageSwitched)
)

// Phases is the closed vocabulary of rotation phases.
var Phases = []Phase{PhaseSingle, PhasePrepared, PhaseSwitched}

var (
	leafNamePattern    = regexp.MustCompile(`^leaf-[0-9a-f]{16}$`)
	fingerprintPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Phase is the phase the record's structure describes, or "" for a record
// no phase describes.
func (st State) Phase() Phase {
	phase, err := describe(st)
	if err != nil {
		return ""
	}
	return phase
}

// describe returns the phase a record's structure describes, or why no
// phase does. The structure is the generations, which of them are retired,
// trusted and serving, the rotation and the leaf's file name. The code has
// only ever written records it describes: generations numbered from 1 in
// order, every one either trusted or retired, and a rotation exactly when two
// are trusted.
func describe(st State) (Phase, error) {
	if st.Version != 1 {
		return "", fmt.Errorf("version %d is not 1", st.Version)
	}
	if err := checkGenerations(st); err != nil {
		return "", err
	}
	if err := checkTrust(st); err != nil {
		return "", err
	}
	if st.Leaf != "" && !leafNamePattern.MatchString(st.Leaf) {
		return "", fmt.Errorf("leaf name %q is not a leaf file name", st.Leaf)
	}
	return describeRotation(st)
}

// checkGenerations requires generations numbered from 1 in order, each with
// a fingerprint and an expiry.
func checkGenerations(st State) error {
	if len(st.Generations) == 0 {
		return errors.New("no generations")
	}
	for i, g := range st.Generations {
		if g.ID != i+1 {
			return fmt.Errorf("generation %d is listed where generation %d belongs", g.ID, i+1)
		}
		if g.Retired && g.Fingerprint == "" && g.NotAfter.IsZero() {
			// A retired generation replace found neither in the refused
			// record nor as a certificate file (replaceable).
			continue
		}
		if !fingerprintPattern.MatchString(g.Fingerprint) {
			return fmt.Errorf("generation %d has no SHA-256 fingerprint", g.ID)
		}
		if g.NotAfter.IsZero() {
			return fmt.Errorf("generation %d has no expiry", g.ID)
		}
	}
	return nil
}

// checkTrust requires a serving generation that is not retired, one or two
// distinct trusted generations that are not retired, and every other
// generation retired.
func checkTrust(st State) error {
	serving := st.generation(st.Serving)
	if serving == nil {
		return fmt.Errorf("serving generation %d is not recorded", st.Serving)
	}
	if serving.Retired {
		return fmt.Errorf("serving generation %d is retired", st.Serving)
	}
	if n := len(st.Trusted); n != 1 && n != 2 {
		return fmt.Errorf("%d trusted generations; a record trusts one, or two during a rotation", n)
	}
	for i, id := range st.Trusted {
		g := st.generation(id)
		if g == nil {
			return fmt.Errorf("trusted generation %d is not recorded", id)
		}
		if g.Retired {
			return fmt.Errorf("trusted generation %d is retired", id)
		}
		if slices.Index(st.Trusted, id) != i {
			return fmt.Errorf("generation %d is trusted twice", id)
		}
	}
	for _, g := range st.Generations {
		if !g.Retired && !slices.Contains(st.Trusted, g.ID) {
			return fmt.Errorf("generation %d is neither trusted nor retired", g.ID)
		}
	}
	return nil
}

// describeRotation derives the phase from the trusted generations, the
// serving one and the rotation, once checkTrust has passed.
func describeRotation(st State) (Phase, error) {
	r := st.Rotation
	if len(st.Trusted) == 1 {
		if r != nil {
			return "", fmt.Errorf("rotation %s from %d to %d is recorded, but only generation %d is trusted",
				r.Stage, r.From, r.To, st.Trusted[0])
		}
		return PhaseSingle, nil
	}
	if r == nil {
		return "", fmt.Errorf("generations %d and %d are trusted with no rotation recorded", st.Trusted[0], st.Trusted[1])
	}
	if r.From != st.Trusted[0] || r.To != st.Trusted[1] {
		return "", fmt.Errorf("rotation from %d to %d does not match the trusted generations %v", r.From, r.To, st.Trusted)
	}
	if r.To != len(st.Generations) {
		return "", fmt.Errorf("rotation target %d is not the newest generation", r.To)
	}
	if r.PreparedAt.IsZero() {
		return "", errors.New("rotation has no preparation time")
	}
	switch {
	case r.Stage != StagePrepared && r.Stage != StageSwitched:
		return "", fmt.Errorf("rotation stage %q is not prepared or switched", r.Stage)
	case r.Stage == StagePrepared && st.Serving == r.From:
		return PhasePrepared, nil
	case r.Stage == StageSwitched && st.Serving == r.To:
		return PhaseSwitched, nil
	}
	return "", fmt.Errorf("rotation stage %s does not match serving generation %d", r.Stage, st.Serving)
}

// check refuses a record no phase describes, naming what is wrong.
func check(st State) error {
	if _, err := describe(st); err != nil {
		return fmt.Errorf("pki: corrupt state: %w", err)
	}
	return nil
}

// checkFiles refuses a record whose trusted generations' certificates are
// not the ones it names: each must have the recorded fingerprint and expiry.
// It reads no key, so a read of the serving leaf succeeds whenever the record
// and its certificates are valid; checkKeys and the operation about to use a
// key check it. A retired generation's kept certificate, when it can be
// read, must also have the recorded fingerprint and expiry; a retired
// generation whose certificate is gone, or whose history replace could not
// recover (no fingerprint and no expiry), is not compared.
func (s *Store) checkFiles(st State) error {
	for _, g := range st.Generations {
		if !g.Retired || g.Fingerprint == "" && g.NotAfter.IsZero() {
			continue
		}
		cert, err := s.caCertificate(g.ID)
		if err != nil {
			continue
		}
		if Fingerprint(cert) != g.Fingerprint {
			return filesError("generation %d certificate %s.pem does not have the recorded fingerprint", g.ID, caName(g.ID))
		}
		if !cert.NotAfter.Equal(g.NotAfter) {
			return filesError("generation %d certificate %s.pem does not have the recorded expiry", g.ID, caName(g.ID))
		}
	}
	for _, id := range st.Trusted {
		g, name := st.generation(id), caName(id)
		data, err := privatefs.ReadFile(filepath.Join(s.dir, name+".pem"), maxPEM)
		var cert *x509.Certificate
		if err == nil {
			cert, err = ParseCertificate(data)
		}
		if err != nil {
			return filesError("generation %d certificate %s.pem is unreadable: %v", id, name, err)
		}
		if Fingerprint(cert) != g.Fingerprint {
			return filesError("generation %d certificate %s.pem does not have the recorded fingerprint", id, name)
		}
		if !cert.NotAfter.Equal(g.NotAfter) {
			return filesError("generation %d certificate %s.pem does not have the recorded expiry", id, name)
		}
	}
	return nil
}

// checkKeys reports every trusted generation's key that cannot be read, or
// does not match its certificate, naming the true cause. A missing key is
// the lost-key state: issuance from it fails, and replace recovers; it is not
// reported here. Ensure runs it after its leaf and trust work.
func (s *Store) checkKeys(st State) error {
	var errs []error
	for _, id := range st.Trusted {
		name := caName(id)
		key, err := privatefs.ReadFile(filepath.Join(s.dir, name+".key"), maxPEM)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("pki: certificate authority key: generation %d key %s.key is unreadable: %w",
				id, name, err))
			continue
		}
		cert, err := privatefs.ReadFile(filepath.Join(s.dir, name+".pem"), maxPEM)
		if err == nil {
			_, _, err = Parse(Pair{Certificate: cert, Key: key})
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("pki: certificate authority key: generation %d key %s.key does not match its "+
				"certificate: %w", id, name, err))
		}
	}
	return errors.Join(errs...)
}

// caFilePattern is the only form of CA file name the store writes:
// ca-<N>.pem and ca-<N>.key, N a positive number without leading zeros.
var caFilePattern = regexp.MustCompile(`^ca-([1-9][0-9]{0,8})\.(pem|key)$`)

// maxReplaceableGeneration bounds the history replace rebuilds. A yearly
// rotation reaches it in ten thousand years; a higher number is a hand edit.
const maxReplaceableGeneration = 10000

// caFileGeneration returns the generation a CA file name belongs to, or 0
// for a name the store does not write.
func caFileGeneration(name string) int {
	m := caFilePattern.FindStringSubmatch(name)
	if m == nil {
		return 0
	}
	id, err := strconv.Atoi(m[1])
	if err != nil {
		return 0
	}
	return id
}

// replaceable is the record ReplaceCA builds on when no phase describes st.
// It keeps st's names and lists generations 1 to N, every one retired, where
// N is the highest generation st records or a ca-N file in the store
// belongs to, so the new generation is numbered after all of them and no
// existing CA file is overwritten. Each generation takes its fingerprint and
// expiry from its kept certificate file, else from st where st records them;
// one found in neither keeps neither. Retiring them all lets the destroy
// pass remove every one of their keys; their certificates stay as history.
func (s *Store) replaceable(st State) (State, error) {
	highest := 0
	for _, g := range st.Generations {
		highest = max(highest, g.ID)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return State{}, err
	}
	for _, entry := range entries {
		highest = max(highest, caFileGeneration(entry.Name()))
	}
	if highest > maxReplaceableGeneration {
		return State{}, fmt.Errorf("pki: corrupt state: generation %d is beyond the %d replace can rebuild; "+
			"restore state.json from a backup", highest, maxReplaceableGeneration)
	}
	base := State{Version: 1, Names: st.Names}
	for id := 1; id <= highest; id++ {
		g := Generation{ID: id, Retired: true}
		if cert, err := s.caCertificate(id); err == nil {
			g.Fingerprint, g.NotAfter = Fingerprint(cert), cert.NotAfter
		} else if i := slices.IndexFunc(st.Generations, func(r Generation) bool {
			return r.ID == id && fingerprintPattern.MatchString(r.Fingerprint) && !r.NotAfter.IsZero()
		}); i >= 0 {
			g.Fingerprint, g.NotAfter = st.Generations[i].Fingerprint, st.Generations[i].NotAfter
		}
		base.Generations = append(base.Generations, g)
	}
	return base, nil
}

func filesError(format string, args ...any) error {
	return fmt.Errorf("pki: certificate authority files do not match state: "+format, args...)
}
