package pki

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
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
	if len(st.Generations) == 0 {
		return "", errors.New("no generations")
	}
	for i, g := range st.Generations {
		if g.ID != i+1 {
			return "", fmt.Errorf("generation %d is listed where generation %d belongs", g.ID, i+1)
		}
		if !fingerprintPattern.MatchString(g.Fingerprint) {
			return "", fmt.Errorf("generation %d has no SHA-256 fingerprint", g.ID)
		}
		if g.NotAfter.IsZero() {
			return "", fmt.Errorf("generation %d has no expiry", g.ID)
		}
	}
	serving := st.generation(st.Serving)
	if serving == nil {
		return "", fmt.Errorf("serving generation %d is not recorded", st.Serving)
	}
	if serving.Retired {
		return "", fmt.Errorf("serving generation %d is retired", st.Serving)
	}
	if n := len(st.Trusted); n != 1 && n != 2 {
		return "", fmt.Errorf("%d trusted generations; a record trusts one, or two during a rotation", n)
	}
	for i, id := range st.Trusted {
		g := st.generation(id)
		if g == nil {
			return "", fmt.Errorf("trusted generation %d is not recorded", id)
		}
		if g.Retired {
			return "", fmt.Errorf("trusted generation %d is retired", id)
		}
		if slices.Index(st.Trusted, id) != i {
			return "", fmt.Errorf("generation %d is trusted twice", id)
		}
	}
	for _, g := range st.Generations {
		if !g.Retired && !slices.Contains(st.Trusted, g.ID) {
			return "", fmt.Errorf("generation %d is neither trusted nor retired", g.ID)
		}
	}
	if st.Leaf != "" && !leafNamePattern.MatchString(st.Leaf) {
		return "", fmt.Errorf("leaf name %q is not a leaf file name", st.Leaf)
	}
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
