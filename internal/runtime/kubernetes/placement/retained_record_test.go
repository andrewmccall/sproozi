package placement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/google/uuid"
)

const recordedDigest = "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"

// recordedWorkspace names the workspace fact.
const recordedWorkspace = "workspace"

// The names of the other facts, and of a first visit.
const (
	factQuiet    = "quiet"
	factTurnless = "turnless"
	factExited   = "exited"
	factOutput   = "output"
	factFenced   = "fenced"
	kindFirst    = "first"
)

// recordedFacts are the retained record's facts beyond its launch and
// reference, each set or not, in the order describeRetained names them.
var recordedFacts = []struct {
	name string
	set  func(*retained)
}{
	{recordedWorkspace, func(r *retained) { r.WorkspaceDigest = recordedDigest }},
	{factQuiet, func(r *retained) { r.NativeQuiet = true }},
	{factTurnless, func(r *retained) { r.Turnless = true }},
	{factExited, func(r *retained) { r.Exited = &session.Exit{Code: 1, Cause: harness.NativeExitSelf} }},
	{factOutput, func(r *retained) { r.OutputDigest = recordedDigest }},
	{factFenced, func(r *retained) { r.WriterFenced = true }},
	{"aborted", func(r *retained) { r.PreparationAborted = true }},
}

func describeRetained(r retained) string {
	var facts []string
	for _, fact := range []struct {
		name string
		set  bool
	}{{"reference", r.Reference.UID != ""}, {recordedWorkspace, r.WorkspaceDigest != ""}, {factQuiet, r.NativeQuiet},
		{factTurnless, r.Turnless}, {factExited, r.Exited != nil}, {factOutput, r.OutputDigest != ""},
		{factFenced, r.WriterFenced}, {"aborted", r.PreparationAborted}} {
		if fact.set {
			facts = append(facts, fact.name)
		}
	}
	if len(facts) == 0 {
		return "launch-only"
	}
	return strings.Join(facts, "+")
}

func describeRecordError(err error) string {
	var contradicted *contradiction
	if errors.As(err, &contradicted) {
		return "contradicts (" + strings.Join(contradicted.reasons, "; ") + ")"
	}
	var rejection *profile.Rejection
	if errors.As(err, &rejection) {
		return string(rejection.Code)
	}
	return "error: " + err.Error()
}

// recordedPhase is the session registration a retained record sits beside:
// none, or one driven through the registry to phase, with a first visit's
// conversation bound or not.
type recordedPhase struct {
	phase registry.Phase
	bound bool
}

func (r recordedPhase) String() string {
	if r.phase == "" {
		return "none"
	}
	if r.bound {
		return string(r.phase) + "+conversation"
	}
	return string(r.phase)
}

func recordedPhases(first bool) []recordedPhase {
	phases := []recordedPhase{{}}
	for _, phase := range []registry.Phase{registry.Creating, registry.Bound, registry.Prepared, registry.Active, registry.Stopping, registry.Stopped, registry.Cancelled} {
		phases = append(phases, recordedPhase{phase: phase})
		if first && phase != registry.Creating && phase != registry.Bound && phase != registry.Prepared {
			phases = append(phases, recordedPhase{phase: phase, bound: true})
		}
	}
	return phases
}

// seedRegistration drives l's registration through the registry to want and
// returns its reference.
func seedRegistration(t *testing.T, p *Placement, l session.Launch, want recordedPhase) registry.Reference {
	t.Helper()
	ctx := context.Background()
	ceilings, err := p.liveProfileInput(ctx, l, l.CheckpointDigest == "")
	if err != nil {
		t.Fatal(err)
	}
	registration := p.registration(l, ceilings)
	if l.CheckpointDigest == "" {
		registration.WorkspaceDigest = recordedDigest
	}
	r, err := p.registry.Create(ctx, registration)
	if err != nil {
		t.Fatal(err)
	}
	ref, run, pod := r.Reference, uuid.NewString(), uuid.NewString()
	steps := []struct {
		phase registry.Phase
		call  func() (registry.Record, error)
	}{
		{registry.Bound, func() (registry.Record, error) { return p.registry.BindRun(ctx, ref, run) }},
		{registry.Prepared, func() (registry.Record, error) {
			if _, err := p.registry.BindPod(ctx, ref, pod); err != nil {
				return registry.Record{}, err
			}
			return p.registry.Prepared(ctx, ref, pod)
		}},
		{registry.Active, func() (registry.Record, error) {
			r, err := p.registry.Activate(ctx, ref)
			if err == nil && want.bound {
				r, err = p.registry.BindNative(ctx, ref, fixtureThread)
			}
			return r, err
		}},
		{registry.Cancelled, func() (registry.Record, error) { return p.registry.Cancel(ctx, ref) }},
		{registry.Stopping, func() (registry.Record, error) { return p.registry.Stop(ctx, ref) }},
		{registry.Stopped, func() (registry.Record, error) {
			digest := recordedDigest
			if l.CheckpointDigest == "" && !want.bound {
				digest = ""
			}
			seals := map[api.CapabilityKind]budget.TrustedUsage{api.CapabilityModelInference: {}}
			return p.registry.CompleteStop(ctx, ref, digest, seals)
		}},
	}
	for _, step := range steps {
		if r.Phase == want.phase {
			break
		}
		if step.phase == registry.Cancelled && want.phase != registry.Cancelled {
			continue
		}
		if r, err = step.call(); err != nil {
			t.Fatalf("%s: %v", step.phase, err)
		}
	}
	if r.Phase != want.phase {
		t.Fatalf("seeded %s, want %s", r.Phase, want.phase)
	}
	return ref
}

// recordedOutcome seeds a retained record holding facts beside a
// registration in phase and describes what loading it, reading it with its
// registration and saving it again do.
func recordedOutcome(t *testing.T, p *Placement, l session.Launch, facts retained, phase recordedPhase) string {
	t.Helper()
	ctx := context.Background()
	l.ExecutionID, l.AllocationID, l.OperationID = uuid.NewString(), uuid.NewString(), uuid.NewString()
	facts.Launch = l
	if phase.phase != "" {
		facts.Reference = seedRegistration(t, p, l, phase)
	}
	data, err := json.Marshal(facts)
	if err != nil {
		t.Fatal(err)
	}
	// A plain private write: the durable write is not what is recorded.
	if err = os.WriteFile(filepath.Join(p.config.Directory, l.AllocationID+recordSuffix), data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, loadErr := p.load(l)
	load := "ok " + describeRetained(loaded)
	var contradicted *contradiction
	if errors.As(loadErr, &contradicted) {
		load = describeRecordError(loadErr) + " keeps " + describeRetained(loaded)
	} else if loadErr != nil {
		load = describeRecordError(loadErr)
	}
	read := "ok"
	local, r, readErr := p.retained(ctx, l)
	if readErr != nil {
		read = describeRecordError(readErr)
	} else {
		read += " " + describeRetained(local) + " " + string(r.Phase)
	}
	save := "ok"
	if err := p.save(facts, registrationOf(t, p, facts)); err != nil {
		save = describeRecordError(err)
	}
	settleCase(t, p, l, readErr == nil, local, r)
	return fmt.Sprintf("load -> %s | read -> %s | save -> %s", load, read, save)
}

// registrationOf reads the registration r names, or nil.
func registrationOf(t *testing.T, p *Placement, r retained) *registry.Record {
	t.Helper()
	if r.Reference.UID == "" {
		return nil
	}
	record, err := p.registry.Get(context.Background(), r.Reference)
	if err != nil {
		t.Fatal(err)
	}
	return &record
}

// settleCase checks what an operation that observes or ends the visit
// reads (observing; settling then saves it): never a
// contradiction, only a record valid beside its registration, the very
// record a refusing read returns when that read accepts it, and one whose
// check is settled (checking it again forgets nothing).
func settleCase(t *testing.T, p *Placement, l session.Launch, valid bool, strict retained, strictRecord registry.Record) {
	t.Helper()
	local, r, err := p.observing(context.Background(), l)
	var contradicted *contradiction
	if errors.As(err, &contradicted) {
		t.Errorf("%s: observing read a contradiction: %v", l.AllocationID, err)
	}
	if err != nil {
		return
	}
	if checked, err := local.check(&r); err != nil || !reflect.DeepEqual(checked, local) {
		t.Errorf("observing read a record its registration contradicts: %+v %v", local, err)
	}
	if valid && strictRecord.Reference.UID != "" && !reflect.DeepEqual(local, strict) {
		t.Errorf("observing read %+v of a valid record read as %+v", local, strict)
	}
}

// Every combination of the retained record's facts, for a resumed and a
// first visit, beside no registration and beside one in every phase, loads,
// reads with its registration and saves as testdata/retained-records.txt
// records. The file was first written from the placement before the record
// was validated (ADR 0058), so its changes since are the refusals the
// validation added.
func TestRetainedRecordsMatchTheRecordedTable(t *testing.T) {
	t.Parallel()
	p, _, _, resumed, _ := setup(t)
	first := resumed
	first.ThreadID, first.CheckpointDigest = "", ""
	var cases []func() string
	for _, launch := range []struct {
		name string
		l    session.Launch
	}{{"resumed", resumed}, {kindFirst, first}} {
		for mask := range 1 << len(recordedFacts) {
			var facts retained
			for i, fact := range recordedFacts {
				if mask&(1<<i) != 0 {
					fact.set(&facts)
				}
			}
			for _, phase := range recordedPhases(launch.l.CheckpointDigest == "") {
				cases = append(cases, func() string {
					return fmt.Sprintf("%s %s | %s | %s", launch.name, describeRetained(facts), phase,
						recordedOutcome(t, p, launch.l, facts, phase))
				})
			}
		}
	}
	// Each case has its own launch, so they run side by side; the durable
	// writes dominate the time.
	lines := make([]string, len(cases))
	var wg sync.WaitGroup
	next := make(chan int)
	for range 16 {
		wg.Go(func() {
			for i := range next {
				lines[i] = cases[i]()
			}
		})
	}
	for i := range cases {
		next <- i
	}
	close(next)
	wg.Wait()
	compareRecorded(t, "testdata/retained-records.txt", lines)
}
