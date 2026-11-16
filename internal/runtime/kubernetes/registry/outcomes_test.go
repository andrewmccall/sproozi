package registry

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var updateOutcomes = flag.Bool("update-outcomes", false, "rewrite testdata/mutator-outcomes.txt from the current code")

const outcomesFile = "testdata/mutator-outcomes.txt"

const (
	outcomeRun        = fixtureRunUID
	outcomeOtherRun   = "other-run"
	outcomePod        = "pod-a"
	outcomeOtherPod   = "pod-b"
	outcomeNative     = "12345678-1234-1234-1234-123456789abc"
	outcomeOtherUUID  = "87654321-4321-4321-4321-cba987654321"
	outcomeCheckpoint = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
)

// outcomeKind is the shape of registration a state carries: one resumed from
// a checkpoint, and a first visit from a Git workspace with its native
// identity not yet bound, or already bound.
type outcomeKind struct {
	name string
	reg  func() Registration
}

func outcomeKinds() []outcomeKind {
	initial := func(native string) func() Registration {
		return func() Registration {
			r := registration()
			r.NativeUUID, r.CheckpointDigest, r.WorkspaceDigest = native, "", strings.Repeat("c", 64)
			return r
		}
	}
	return []outcomeKind{{"checkpoint", registration}, {"initial", initial("")}, {"initial-bound", initial(outcomeNative)}}
}

// outcomeClock is when a mutator runs relative to the registration's
// deadline: before it, after it, or for a registration without one.
type outcomeClock struct {
	name     string
	deadline time.Time
	now      time.Time
}

var outcomeClocks = []outcomeClock{
	{"early", testNow.Add(time.Hour), testNow},
	{"late", testNow.Add(time.Hour), testNow.Add(2 * time.Hour)},
	{"none", time.Time{}, testNow.Add(100 * 365 * 24 * time.Hour)},
}

func outcomeSeals(units int64) map[api.CapabilityKind]budget.TrustedUsage {
	return map[api.CapabilityKind]budget.TrustedUsage{api.CapabilityModelInference: {Units: units, CostMicros: units}}
}

// stopReceipt is the receipt a valid stop of reg retains.
func outcomeStopReceipt(reg Registration, units int64) *StopReceipt {
	digest := outcomeCheckpoint
	if reg.WorkspaceDigest != "" && reg.NativeUUID == "" {
		digest = ""
	}
	return &StopReceipt{CheckpointDigest: digest, Seals: outcomeSeals(units)}
}

// outcomeStates lists every stored state the registry accepts for reg: each
// phase with each combination of bound run, bound Pod and receipt that
// validateStored allows.
func outcomeStates(reg Registration) []stored {
	var states []stored
	for _, phase := range []Phase{Creating, Bound, Prepared, Active, Stopping, Stopped, Cancelled} {
		for _, run := range []string{"", outcomeRun} {
			for _, pod := range []string{"", outcomePod} {
				receipts := []*StopReceipt{nil, {Seals: outcomeSeals(1)}}
				if phase == Stopped {
					receipts = []*StopReceipt{nil, outcomeStopReceipt(reg, 1)}
				}
				for _, receipt := range receipts {
					s := stored{Version: 1, Registration: storedRegistration{Registration: reg}, RunUID: run, PodUID: pod, Phase: phase, Receipt: receipt}
					if validateStored(s) == nil {
						states = append(states, s)
					}
				}
			}
		}
	}
	return states
}

type outcomeOp struct {
	name string
	call func(ctx context.Context, s *Store, ref Reference, reg Registration) (Record, error)
}

func outcomeOps() []outcomeOp {
	return []outcomeOp{
		{"create", func(ctx context.Context, s *Store, _ Reference, reg Registration) (Record, error) {
			// The caller's intent never names the native identity a first visit binds later.
			if reg.WorkspaceDigest != "" {
				reg.NativeUUID = ""
			}
			return s.Create(ctx, reg)
		}},
		{"create-changed", func(ctx context.Context, s *Store, _ Reference, reg Registration) (Record, error) {
			reg.OperationID = "move-2"
			return s.Create(ctx, reg)
		}},
		{"bind-run", func(ctx context.Context, s *Store, ref Reference, _ Registration) (Record, error) {
			return s.BindRun(ctx, ref, outcomeRun)
		}},
		{"bind-run-other", func(ctx context.Context, s *Store, ref Reference, _ Registration) (Record, error) {
			return s.BindRun(ctx, ref, outcomeOtherRun)
		}},
		{"bind-pod", func(ctx context.Context, s *Store, ref Reference, _ Registration) (Record, error) {
			return s.BindPod(ctx, ref, outcomePod)
		}},
		{"bind-pod-other", func(ctx context.Context, s *Store, ref Reference, _ Registration) (Record, error) {
			return s.BindPod(ctx, ref, outcomeOtherPod)
		}},
		{"prepared", func(ctx context.Context, s *Store, ref Reference, _ Registration) (Record, error) {
			return s.Prepared(ctx, ref, outcomePod)
		}},
		{"prepared-other", func(ctx context.Context, s *Store, ref Reference, _ Registration) (Record, error) {
			return s.Prepared(ctx, ref, outcomeOtherPod)
		}},
		{"activate", func(ctx context.Context, s *Store, ref Reference, _ Registration) (Record, error) {
			return s.Activate(ctx, ref)
		}},
		{"bind-native", func(ctx context.Context, s *Store, ref Reference, _ Registration) (Record, error) {
			return s.BindNative(ctx, ref, outcomeNative)
		}},
		{"bind-native-other", func(ctx context.Context, s *Store, ref Reference, _ Registration) (Record, error) {
			return s.BindNative(ctx, ref, outcomeOtherUUID)
		}},
		{"stop", func(ctx context.Context, s *Store, ref Reference, _ Registration) (Record, error) {
			return s.Stop(ctx, ref)
		}},
		{"complete-stop", func(ctx context.Context, s *Store, ref Reference, reg Registration) (Record, error) {
			r := outcomeStopReceipt(reg, 1)
			return s.CompleteStop(ctx, ref, r.CheckpointDigest, r.Seals)
		}},
		{"complete-stop-other", func(ctx context.Context, s *Store, ref Reference, reg Registration) (Record, error) {
			r := outcomeStopReceipt(reg, 2)
			return s.CompleteStop(ctx, ref, r.CheckpointDigest, r.Seals)
		}},
		{"cancel", func(ctx context.Context, s *Store, ref Reference, _ Registration) (Record, error) {
			return s.Cancel(ctx, ref)
		}},
		{"complete-cancel", func(ctx context.Context, s *Store, ref Reference, _ Registration) (Record, error) {
			return s.CompleteCancel(ctx, ref, outcomeSeals(1))
		}},
		{"complete-cancel-other", func(ctx context.Context, s *Store, ref Reference, _ Registration) (Record, error) {
			return s.CompleteCancel(ctx, ref, outcomeSeals(2))
		}},
	}
}

func describeState(s *stored) string {
	if s == nil {
		return "none"
	}
	receipt := "none"
	if s.Receipt != nil {
		receipt = fmt.Sprintf("units=%d", s.Receipt.Seals[api.CapabilityModelInference].Units)
		if s.Receipt.CheckpointDigest != "" {
			receipt += "+checkpoint"
		}
	}
	native := "unbound"
	if s.Registration.NativeUUID == outcomeOtherUUID {
		native = "other"
	} else if s.Registration.NativeUUID != "" {
		native = "bound"
	}
	return fmt.Sprintf("%s run=%q pod=%q native=%s receipt=%s", s.Phase, s.RunUID, s.PodUID, native, receipt)
}

func describeError(err error) string {
	switch {
	case errors.Is(err, ErrExpired):
		return "expired"
	case errors.Is(err, ErrGone):
		return "gone"
	case errors.Is(err, ErrConflict):
		return "conflict"
	}
	return "error: " + err.Error()
}

// outcome runs op against a fresh store holding state (none: no record) at
// clock and describes the result: the refusal, or the stored state after the
// call and whether the call wrote it.
func outcome(t *testing.T, kind outcomeKind, clock outcomeClock, state *stored, op outcomeOp) string {
	t.Helper()
	s, kube := registry(t, nil)
	s.now = func() time.Time { return clock.now }
	reg := kind.reg()
	reg.OriginalDeadline = clock.deadline
	ctx := context.Background()
	name := NameForRun(reg.IntendedRunName)
	ref := Reference{Name: name, UID: "seeded-record", Epoch: reg.Epoch}
	seeded := ""
	if state != nil {
		document, err := encode(*state)
		if err != nil {
			t.Fatal(err)
		}
		cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: fixtureNamespace, UID: types.UID(ref.UID),
			Labels: map[string]string{registryLabel: enabledValue}}, Data: map[string]string{dataKey: document}}
		if err = kube.Create(ctx, cm); err != nil {
			t.Fatal(err)
		}
		seeded = cm.ResourceVersion
	}
	_, err := op.call(ctx, s, ref, reg)
	if err != nil {
		return describeError(err)
	}
	var cm corev1.ConfigMap
	if err = kube.Get(ctx, client.ObjectKey{Namespace: fixtureNamespace, Name: name}, &cm); err != nil {
		t.Fatal(err)
	}
	after, err := read(&cm)
	if err != nil {
		t.Fatal(err)
	}
	written := "unchanged"
	if cm.ResourceVersion != seeded {
		written = "written"
	}
	afterState := stored{Registration: storedRegistration{Registration: after.Registration}, RunUID: after.RunUID, PodUID: after.PodUID, Phase: after.Phase, Receipt: after.Receipt}
	return describeState(&afterState) + " " + written
}

// Every mutator, from every stored state the registry accepts, before and
// after the deadline and without one, has the outcome recorded in
// testdata/mutator-outcomes.txt. The file was generated from the registry
// before its transitions moved into a table (ADR 0058), so the table changed
// no accepted phase, written phase or refusal.
func TestMutatorOutcomesMatchTheRecordedTable(t *testing.T) {
	var lines []string
	for _, kind := range outcomeKinds() {
		for _, clock := range outcomeClocks {
			reg := kind.reg()
			reg.OriginalDeadline = clock.deadline
			states := []*stored{nil}
			for _, s := range outcomeStates(reg) {
				states = append(states, &s)
			}
			for _, state := range states {
				for _, op := range outcomeOps() {
					lines = append(lines, fmt.Sprintf("%s %s | %s | %s -> %s", kind.name, clock.name, describeState(state), op.name,
						outcome(t, kind, clock, state, op)))
				}
			}
		}
	}
	got := strings.Join(lines, "\n") + "\n"
	if *updateOutcomes {
		if err := os.MkdirAll(filepath.Dir(outcomesFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(outcomesFile, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(outcomesFile)
	if err != nil {
		t.Fatal(err)
	}
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
