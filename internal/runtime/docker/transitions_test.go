package docker

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/testutil"
	"github.com/google/uuid"
)

// writeRaw stores a record without save's validation, as a damaged or older
// file on disk would be.
func writeRaw(t *testing.T, p *Placement, r placementRecord) {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.write(operationKey(r.Launch)+".json", data); err != nil {
		t.Fatal(err)
	}
}

func fixtureReceipt(r placementRecord, i session.Incarnation) *session.Receipt {
	return &session.Receipt{Launch: r.Launch, Incarnation: i, Usage: map[string]budget.TrustedUsage{modelFixtureCapability: {Units: 1, CostMicros: 1}}}
}

// A record whose phase or flags contradict each other is refused on read and
// on write, like the Kubernetes registration's TestCorruptRegistryFailsClosed.
func TestCorruptPlacementRecordFailsClosed(t *testing.T) {
	t.Parallel()
	p, _, _, l, i := activeFixture(t, testutil.PrivateDir(t))
	valid, err := p.load(l)
	if err != nil {
		t.Fatal(err)
	}
	digest := strings.Repeat("d", 64)
	cases := map[string]func(r *placementRecord){
		"unknown phase":                    func(r *placementRecord) { r.Phase = "paused" },
		"prepared without its agent":       func(r *placementRecord) { r.Phase, r.Agent = phasePrepared, nil },
		"active without its volume":        func(r *placementRecord) { r.Volume = nil },
		"active marked stopped":            func(r *placementRecord) { r.ActualStopped = true },
		"active marked quiet":              func(r *placementRecord) { r.NativeQuiet = true },
		"active with an exit":              func(r *placementRecord) { r.Exited = &session.Exit{Code: 1} },
		"active with a receipt":            func(r *placementRecord) { r.Receipt = fixtureReceipt(*r, i) },
		"active marked removing":           func(r *placementRecord) { r.Removing = true },
		"stopped without an actual stop":   func(r *placementRecord) { r.Phase, r.Receipt = phaseStopped, fixtureReceipt(*r, i) },
		"stopped without a receipt":        func(r *placementRecord) { r.Phase, r.ActualStopped = phaseStopped, true },
		"aborted without an actual stop":   func(r *placementRecord) { r.Phase = phaseAborted },
		"removed without removing":         func(r *placementRecord) { r.Phase, r.ActualStopped, r.Removed = phaseAborted, true, true },
		"removing an aborted unsealed run": func(r *placementRecord) { r.Phase, r.ActualStopped, r.Removing = phaseAborted, true, true },
		"receipt for another incarnation": func(r *placementRecord) {
			r.Phase, r.ActualStopped, r.Receipt = phaseStopping, true, fixtureReceipt(*r, session.Incarnation{ExecutionID: i.ExecutionID, ResourceUID: strings.Repeat("e", 64)})
		},
		"receipt before the actual stop": func(r *placementRecord) { r.Phase, r.Receipt = phaseStopping, fixtureReceipt(*r, i) },
		"checkpoint without a receipt":   func(r *placementRecord) { r.Phase, r.ActualStopped, r.CheckpointDigest = phaseStopping, true, digest },
		"malformed checkpoint digest": func(r *placementRecord) {
			r.Phase, r.ActualStopped, r.Receipt, r.CheckpointDigest = phaseStopping, true, fixtureReceipt(*r, i), "not-a-digest"
		},
		"turnless resumed visit":      func(r *placementRecord) { r.Phase, r.NativeQuiet, r.Turnless = phaseStopping, true, true },
		"bound thread on resumed run": func(r *placementRecord) { r.Phase, r.Thread = phaseStopping, uuid.NewString() },
		"turnless and exited": func(r *placementRecord) {
			r.Phase, r.NativeQuiet, r.Turnless, r.Exited = phaseStopping, true, true, &session.Exit{Code: 1}
		},
		"cancelled preparation with a receipt": func(r *placementRecord) {
			r.Phase, r.ActualStopped, r.Receipt = phasePreparationAborting, true, fixtureReceipt(*r, i)
		},
		"cancelled preparation marked quiet": func(r *placementRecord) { r.Phase, r.NativeQuiet = phasePreparationAborting, true },
	}
	for name, corrupt := range cases {
		r := valid
		corrupt(&r)
		if err := p.save(r); err == nil {
			t.Errorf("%s: save accepted the record", name)
		}
		writeRaw(t, p, r)
		if _, err := p.load(l); err == nil {
			t.Errorf("%s: load accepted the record", name)
		}
		if _, err := p.Inspect(t.Context(), l); err == nil {
			t.Errorf("%s: inspect accepted the record", name)
		}
		writeRaw(t, p, valid)
	}
	if _, err := p.load(l); err != nil {
		t.Fatalf("restored record refused: %v", err)
	}
}

// Every operation refuses a phase its transition table leaves out, and the
// refusal changes neither the record nor the grant.
func TestPlacementOperationsRefusedOutsideTheirPhases(t *testing.T) {
	t.Parallel()
	type op func(p *Placement, l session.Launch, i session.Incarnation) error
	stop := func(p *Placement, l session.Launch, i session.Incarnation) error {
		_, err := p.Stop(t.Context(), l, i)
		return err
	}
	abort := func(p *Placement, l session.Launch, i session.Incarnation) error {
		_, err := p.Abort(t.Context(), l, i)
		return err
	}
	activate := func(p *Placement, l session.Launch, i session.Incarnation) error {
		return p.Activate(t.Context(), l, i)
	}
	prepare := func(p *Placement, l session.Launch, _ session.Incarnation) error {
		_, err := p.Prepare(t.Context(), l, fixtureArtifact(t))
		return err
	}
	cases := []struct {
		from dockerPhase
		name placementOp
		op   op
	}{
		{phaseCreating, opStop, stop},
		{phaseAborting, opStop, stop},
		{phaseAborted, opStop, stop},
		{phasePreparationAborting, opStop, stop},
		{phaseCreating, opAbort, abort},
		{phasePreparationAborting, opAbort, abort},
		{phaseCreating, opActivate, activate},
		{phaseStopping, opActivate, activate},
		{phaseStopped, opActivate, activate},
		{phaseAborting, opActivate, activate},
		{phaseAborted, opActivate, activate},
		{phasePreparationAborting, opActivate, activate},
		{phaseActivating, opPrepare, prepare},
		{phaseActive, opPrepare, prepare},
		{phaseStopping, opPrepare, prepare},
		{phaseAborted, opPrepare, prepare},
	}
	for _, c := range cases {
		t.Run(string(c.from)+"/"+string(c.name), func(t *testing.T) {
			t.Parallel()
			p, f, a, l, i := activeFixture(t, testutil.PrivateDir(t))
			r, err := p.load(l)
			if err != nil {
				t.Fatal(err)
			}
			r.Phase = c.from
			if c.from == phaseStopped || c.from == phaseAborted {
				if err = p.engine.Stop(t.Context(), *r.Agent); err != nil {
					t.Fatal(err)
				}
				r.ActualStopped, r.Receipt = true, fixtureReceipt(r, i)
				// Keep the containers so the refusal, not a removal, is observed.
				f.failRemove = true
			}
			if err = p.save(r); err != nil {
				t.Fatalf("fixture record invalid: %v", err)
			}
			a.revoked = false
			if err = c.op(p, l, i); err == nil {
				t.Fatalf("%s accepted from %s", c.name, c.from)
			}
			after, err := p.load(l)
			if err != nil || after.Phase != c.from {
				t.Fatalf("refused %s changed the record: %s %v", c.name, after.Phase, err)
			}
			if a.revoked {
				t.Fatalf("refused %s revoked the grant", c.name)
			}
		})
	}
}

// A preparation cancelled after its grant was initialized revokes the grant,
// so every grant record ends revoked.
func TestCancelledPreparationRevokesItsGrant(t *testing.T) {
	t.Parallel()
	data := fixtureArtifact(t)
	l := fixtureLaunch(t, data)
	f := newFakeDocker()
	config := fixtureConfig()
	config.ProfileDirectory = filepath.Join(testutil.PrivateDir(t), "profile")
	// A reserved skill name fails preparation after the grant is initialized.
	writeProfileFixture(t, config.ProfileDirectory, "sproozi-access", "Profile instructions")
	a := &fakeAuthority{engine: f}
	p := openFixtureWith(t, testutil.PrivateDir(t), f, a, config)
	if _, err := p.Prepare(t.Context(), l, data); err == nil {
		t.Fatal("expected failed preparation")
	}
	if a.init != 1 {
		t.Fatalf("fixture did not initialize the grant: %d", a.init)
	}
	a.revokeErr = errors.New("registry unavailable")
	if err := p.AbortPreparation(t.Context(), l); err == nil {
		t.Fatal("cancellation finished without revoking the grant")
	}
	if r, err := p.load(l); err != nil || r.ActualStopped || r.Removing {
		t.Fatalf("unrevoked grant was recorded as stopped: %+v %v", r, err)
	}
	a.revokeErr = nil
	if err := p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	if !a.revoked || a.activate != 0 || a.seals != 0 {
		t.Fatalf("grant after cancelled preparation: revoked=%v activate=%d seals=%d", a.revoked, a.activate, a.seals)
	}
	assertReleasedPreparation(t, p, f, l)
}
