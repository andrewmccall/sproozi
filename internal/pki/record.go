package pki

import (
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

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
// read, must also have the recorded fingerprint and expiry, so a blank entry
// (no fingerprint and no expiry, which replace writes only for a generation
// whose certificate it could not read) is accepted only while that
// certificate is gone or unreadable. A retired generation whose certificate
// is gone is not compared.
func (s *Store) checkFiles(st State) error {
	for _, g := range st.Generations {
		if !g.Retired {
			continue
		}
		cert, err := s.caCertificate(g.ID)
		if err != nil {
			continue
		}
		if g.Fingerprint == "" && g.NotAfter.IsZero() {
			return filesError("generation %d certificate %s.pem is kept, but the record has no fingerprint or expiry for it",
				g.ID, caName(g.ID))
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
// does not match its certificate, naming the true cause, and a missing
// serving key, which the next renewal needs. A missing key of a generation
// that does not serve is the accepted lost-key state and is not reported;
// switching to it fails, and replace recovers. Ensure runs it after its leaf
// and trust work.
func (s *Store) checkKeys(st State) error {
	var errs []error
	for _, id := range st.Trusted {
		name := caName(id)
		key, err := privatefs.ReadFile(filepath.Join(s.dir, name+".key"), maxPEM)
		if os.IsNotExist(err) {
			if id == st.Serving {
				// The next renewal needs it: say so while the leaf is valid.
				errs = append(errs, fmt.Errorf("pki: certificate authority key: generation %d key %s.key is missing; "+
					"the next renewal will fail: recover it, or replace the CA", id, name))
			}
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

// minGenerationBytes is less than any generation entry takes in a saved
// record, so a history longer than maxState/minGenerationBytes can never be
// read back, and replace refuses it before building it.
const minGenerationBytes = 48

// recordSize is the number of bytes commit writes for st.
func recordSize(st State) (int, error) {
	data, err := json.MarshalIndent(st, "", "  ")
	return len(data) + 1, err
}

// growthReserve is the headroom below maxState that a move starting
// something must find. The invariant: prepare, replace and a change of the
// serving names happen only while the record is at most maxState -
// growthReserve bytes, and everything that can then follow before the store
// is single again - the new generation entry, the rotation, a second trusted
// id, a serving id a digit longer, a full-precision preparation time, a
// retired flag, a new leaf name of fixed length - adds at most about 360
// bytes (a generation entry about 185 with its retired flag, the rotation
// about 135 with a nanosecond timestamp, the trusted id and a longer serving
// id about 20). So switch, rollback, retire and renewal with unchanged names
// always commit. The reserve is 1024 bytes, nearly three times that, and
// TestTheReserveCoversWhatFinishingAdds measures it. commit's own size check
// stays as the last guard.
const growthReserve = 1024

// startsWithin refuses a move that starts something when the record st,
// before the move, leaves less than growthReserve below what load reads. It
// runs before the move writes anything.
func startsWithin(st State, what string) error {
	size, err := recordSize(st)
	if err != nil {
		return err
	}
	if size > maxState-growthReserve {
		return fmt.Errorf("pki: refusing to %s: the record is %d bytes, and starting it needs %d bytes below the "+
			"%d bytes load reads; start a new authority and trust it everywhere", what, size, growthReserve, maxState)
	}
	return nil
}

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
	highest, source := 0, ""
	for _, g := range st.Generations {
		if g.ID > highest {
			highest, source = g.ID, fmt.Sprintf("generation %d in state.json", g.ID)
		}
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return State{}, err
	}
	for _, entry := range entries {
		if id := caFileGeneration(entry.Name()); id > highest {
			highest, source = id, entry.Name()
		}
	}
	tooLarge := func(size string) error {
		remedy := "remove " + source + " if Sproozi did not write it"
		if !strings.HasPrefix(source, "ca-") {
			remedy = "correct that id, or restore state.json from a backup"
		}
		return fmt.Errorf("pki: replace would number the new authority after %s, and the %d generations before it "+
			"take %s, leaving less than the %d bytes a new authority needs below the %d bytes load reads; %s, "+
			"and replace again", source, highest, size, growthReserve, maxState, remedy)
	}
	if highest > maxState/minGenerationBytes {
		return State{}, tooLarge(fmt.Sprintf("more than %d bytes", highest*minGenerationBytes))
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
	size, err := recordSize(base)
	if err != nil {
		return State{}, err
	}
	if size > maxState-growthReserve {
		return State{}, tooLarge(fmt.Sprintf("%d bytes", size))
	}
	return base, nil
}

func filesError(format string, args ...any) error {
	return fmt.Errorf("pki: certificate authority files do not match state: "+format, args...)
}
