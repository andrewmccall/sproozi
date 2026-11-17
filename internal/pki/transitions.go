package pki

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

// phaseNone is the phase before Initialize commits a record.
const phaseNone Phase = ""

// phaseRefused is a record no phase describes. Load refuses it, so only
// ReplaceCA, which reads the record without that check, ever applies an event
// to it, and the table lets only replace leave it.
const phaseRefused Phase = "refused"

// event is a request that moves the rotation record between phases.
// Issuing a leaf (Ensure, Renew, and the leaf a switch or rollback issues)
// is not an event: it changes the leaf only, in every phase.
type event string

const (
	// evInitialize creates the first generation, which serves.
	evInitialize event = "initialize"
	// evPrepare creates the next generation and trusts both.
	evPrepare event = "prepare"
	// evSwitch serves from the new generation while both are trusted.
	evSwitch event = "switch"
	// evRollback serves from the previous generation again.
	evRollback event = "rollback"
	// evRetire stops trusting the previous generation and retires it.
	evRetire event = "retire"
	// evReplace retires every generation and trusts only a new one.
	evReplace event = "replace"
)

// transitions declares every phase change the store makes: for each event,
// the phases it may happen in and the phase each leads to. The record has no
// deadline (ADR 0058, rule 5): no event is refused because time passed, and
// no phase waits on time to end. Every phase has an exit: replace from all of
// them and from a refused record, and retire or rollback from the two
// rotation phases.
var transitions = map[event]map[Phase]Phase{
	evInitialize: {phaseNone: PhaseSingle},
	evPrepare:    {PhaseSingle: PhasePrepared},
	evSwitch:     {PhasePrepared: PhaseSwitched},
	evRollback:   {PhaseSwitched: PhasePrepared},
	evRetire:     {PhaseSwitched: PhaseSingle},
	evReplace: {PhaseSingle: PhaseSingle, PhasePrepared: PhaseSingle, PhaseSwitched: PhaseSingle,
		phaseRefused: PhaseSingle},
}

// phase is the record's phase, phaseNone before Initialize, or phaseRefused
// for a record no phase describes.
func (st State) phase() Phase {
	if st.Version == 0 {
		return phaseNone
	}
	if p := st.Phase(); p != "" {
		return p
	}
	return phaseRefused
}

// refused is the error for an event its table does not allow in phase.
func refused(ev event, from Phase) error {
	switch ev {
	case evInitialize:
		return errors.New("pki: certificate authority already initialized")
	case evPrepare:
		return fmt.Errorf("pki: rotation already %s", from)
	case evSwitch:
		return errors.New("pki: no prepared rotation to switch")
	case evRollback:
		return errors.New("pki: no switched rotation to roll back")
	case evRetire:
		return errors.New("pki: rotation must switch before retirement")
	}
	return fmt.Errorf("pki: %s is not allowed in phase %q", ev, from)
}

// admit checks that ev may happen in st's phase, before any side effect
// such as creating a generation's files.
func (st State) admit(ev event) error {
	if _, ok := transitions[ev][st.phase()]; !ok {
		return refused(ev, st.phase())
	}
	return nil
}

// apply records ev: it moves st by the table to the phase transitions
// declares, or refuses an undeclared move and leaves st unchanged. It is the
// only place the record's generations, trust, serving generation and
// rotation change, for live operations and for a retry after a restart
// alike. added is the generation initialize, prepare and replace create;
// its files are already written.
func (st *State) apply(ev event, added *Generation, now time.Time) error {
	from := st.phase()
	to, ok := transitions[ev][from]
	if !ok {
		return refused(ev, from)
	}
	next := st.clone()
	switch ev {
	case evInitialize:
		next = State{Version: 1, Names: next.Names, Generations: []Generation{*added}, Serving: added.ID,
			Trusted: []int{added.ID}}
	case evPrepare:
		next.Generations = append(next.Generations, *added)
		next.Trusted = []int{next.Serving, added.ID}
		next.Rotation = &Rotation{Stage: StagePrepared, From: next.Serving, To: added.ID, PreparedAt: now.UTC()}
	case evSwitch:
		next.Serving, next.Rotation.Stage = next.Rotation.To, StageSwitched
	case evRollback:
		next.Serving, next.Rotation.Stage = next.Rotation.From, StagePrepared
	case evRetire:
		next.generation(next.Rotation.From).Retired = true
		next.Trusted, next.Rotation = []int{next.Rotation.To}, nil
	case evReplace:
		for i := range next.Generations {
			next.Generations[i].Retired = true
		}
		next.Generations = append(next.Generations, *added)
		next.Serving, next.Trusted, next.Rotation = added.ID, []int{added.ID}, nil
	}
	if got, err := describe(next); err != nil || got != to {
		return fmt.Errorf("pki: %s from %s did not produce %s: %v", ev, from, to, err)
	}
	*st = next
	if appliedHook != nil {
		appliedHook(ev, from, to)
	}
	return nil
}

// appliedHook, when a test sets it, observes every move apply makes. It is
// nil in production.
var appliedHook func(ev event, from, to Phase)

func (st State) clone() State {
	out := st
	out.Generations = slices.Clone(st.Generations)
	out.Trusted = slices.Clone(st.Trusted)
	if st.Rotation != nil {
		r := *st.Rotation
		out.Rotation = &r
	}
	return out
}
