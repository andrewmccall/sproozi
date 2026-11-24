package placement

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/harness"
	runtimekube "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// endingPhase brings a prepared visit to one registration phase through the
// public operations.
type endingPhase struct {
	name  string
	phase registry.Phase
	reach func(t *testing.T, p *Placement, e *protocolExecutor, l session.Launch, i session.Incarnation)
}

func activate(t *testing.T, p *Placement, l session.Launch, i session.Incarnation) {
	t.Helper()
	if err := p.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
}

var endingPhases = []endingPhase{
	{"prepared", registry.Prepared, nil},
	{"active", registry.Active, func(t *testing.T, p *Placement, _ *protocolExecutor, l session.Launch, i session.Incarnation) {
		activate(t, p, l, i)
	}},
	// A stop whose capture failed holds a quiet source and a fenced writer
	// in a stopping registration.
	{"stopping", registry.Stopping, func(t *testing.T, p *Placement, e *protocolExecutor, l session.Launch, i session.Incarnation) {
		activate(t, p, l, i)
		e.captureErr = errors.New("capture helper failed")
		if _, err := p.Stop(t.Context(), l, i); err == nil {
			t.Fatal("stop with a failing capture succeeded")
		}
		e.captureErr = nil
	}},
	{"stopped", registry.Stopped, func(t *testing.T, p *Placement, _ *protocolExecutor, l session.Launch, i session.Incarnation) {
		activate(t, p, l, i)
		if _, err := p.Stop(t.Context(), l, i); err != nil {
			t.Fatal(err)
		}
	}},
	{"cancelled", registry.Cancelled, func(t *testing.T, p *Placement, _ *protocolExecutor, l session.Launch, i session.Incarnation) {
		activate(t, p, l, i)
		if _, err := p.Abort(t.Context(), l, i); err != nil {
			t.Fatal(err)
		}
	}},
}

type endingFact struct {
	name   string
	change func(*retained)
}

// endingFacts each change one retained fact the way a corrupt or foreign
// write would: set it when it is clear, or clear it when it is set.
var endingFacts = []endingFact{
	{factQuiet, func(r *retained) { r.NativeQuiet = !r.NativeQuiet }},
	{factTurnless, func(r *retained) { r.Turnless = !r.Turnless }},
	{factExited, func(r *retained) {
		if r.Exited == nil {
			r.Exited = &session.Exit{Code: 1, Cause: harness.NativeExitSelf}
		} else {
			r.Exited = nil
		}
	}},
	{factFenced, func(r *retained) { r.WriterFenced = !r.WriterFenced }},
	{factOutput, func(r *retained) {
		if r.OutputDigest == "" {
			r.OutputDigest = recordedDigest
		} else {
			r.OutputDigest = ""
		}
	}},
	{"workspace", func(r *retained) {
		if r.WorkspaceDigest == "" {
			r.WorkspaceDigest = recordedDigest
		}
	}},
}

type endingOp struct {
	name string
	call func(ctx context.Context, p *Placement, l session.Launch, i session.Incarnation) error
}

// opAbortPreparation names the abort of a preparation among endingOps.
const opAbortPreparation = "abort-preparation"

// opStop names the stop among endingOps.
const opStop = "stop"

// endingOps are the operations that observe or end a visit.
var endingOps = []endingOp{
	{"inspect", func(ctx context.Context, p *Placement, l session.Launch, _ session.Incarnation) error {
		_, err := p.Inspect(ctx, l)
		return err
	}},
	{"exited", func(ctx context.Context, p *Placement, l session.Launch, i session.Incarnation) error {
		_, err := p.Exited(ctx, l, i)
		return err
	}},
	{opStop, func(ctx context.Context, p *Placement, l session.Launch, i session.Incarnation) error {
		_, err := p.Stop(ctx, l, i)
		return err
	}},
	{"abort", func(ctx context.Context, p *Placement, l session.Launch, i session.Incarnation) error {
		_, err := p.Abort(ctx, l, i)
		return err
	}},
	{opAbortPreparation, func(ctx context.Context, p *Placement, l session.Launch, _ session.Incarnation) error {
		return p.AbortPreparation(ctx, l)
	}},
}

// reachedVisit is a visit brought to a phase once; each case copies it.
type reachedVisit struct {
	p    *Placement
	kube *admissionClient
	e    *protocolExecutor
	l    session.Launch
	i    session.Incarnation
}

func reach(t *testing.T, first bool, phase endingPhase) reachedVisit {
	t.Helper()
	p, kube, e, l, data := setup(t)
	if first {
		l = freshLaunch(t, e, l, data, "")
		data = e.initial
	}
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if phase.reach != nil {
		phase.reach(t, p, e, l, i)
	}
	if r := registration(t, p, l); r.Phase != phase.phase {
		t.Fatalf("reached %s, want %s", r.Phase, phase.phase)
	}
	return reachedVisit{p: p, kube: kube, e: e, l: l, i: i}
}

// copyVisit gives a case its own placement holding what v's cluster and
// directory hold, so the cases share the cost of reaching the phase.
func copyVisit(t *testing.T, v reachedVisit) *Placement {
	t.Helper()
	p, kube, _, _, _ := setup(t)
	ctx := t.Context()
	for _, list := range []client.ObjectList{&corev1.ConfigMapList{}, &api.AgentRunList{}, &corev1.PodList{}, &api.BudgetLedgerList{}} {
		if err := v.kube.List(ctx, list); err != nil {
			t.Fatal(err)
		}
		if err := meta.EachListItem(list, func(item runtime.Object) error {
			source := item.(client.Object)
			obj := source.DeepCopyObject().(client.Object)
			obj.SetResourceVersion("")
			if err := kube.Client.Create(ctx, obj); err != nil {
				return err
			}
			status := source.DeepCopyObject().(client.Object)
			status.SetResourceVersion(obj.GetResourceVersion())
			switch status.(type) {
			case *corev1.Pod, *api.AgentRun: // the kinds with a status subresource
				return kube.Status().Update(ctx, status)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(v.p.config.Directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() == "owner.lock" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(v.p.config.Directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(p.config.Directory, entry.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p.exec = &protocolExecutor{kube: kube, data: v.e.data, meta: v.e.meta, initial: v.e.initial, state: v.e.state}
	return p
}

// Every operation that observes or ends a visit, on a visit in each phase
// whose retained record has one fact changed so that it contradicts the
// record or its registration, either settles or fails in a way the visit
// recovers from: it never reports the contradiction, an abort of the same
// incarnation afterwards proves the visit stopped and leaves a valid record,
// and on a stopped or cancelled registration it replays as the registration
// says. The unchanged record is the control. A first visit differs from a
// resumed one only in its turnless fact.
func TestEndingOperationsSettleAContradictedRecord(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"resumed", kindFirst} {
		for _, phase := range endingPhases {
			t.Run(kind+"/"+phase.name, func(t *testing.T) {
				t.Parallel()
				v := reach(t, kind == kindFirst, phase)
				before := registration(t, v.p, v.l)
				stored, err := v.p.load(v.l)
				if err != nil {
					t.Fatal(err)
				}
				facts := append([]endingFact{{"unchanged", func(*retained) {}}}, endingFacts...)
				if kind == kindFirst {
					facts = endingFacts[1:2]
				}
				for _, fact := range facts {
					changed := stored
					fact.change(&changed)
					var contradicted *contradiction
					if _, err := changed.check(&before); fact.name != "unchanged" && !errors.As(err, &contradicted) {
						continue
					}
					for _, op := range endingOps {
						t.Run(fact.name+"/"+op.name, func(t *testing.T) {
							t.Parallel()
							endingCase(t, v, phase, fact.change, op)
						})
					}
				}
			})
		}
	}
}

func endingCase(t *testing.T, v reachedVisit, phase endingPhase, change func(*retained), op endingOp) {
	p := copyVisit(t, v)
	l, i := v.l, v.i
	contradict(t, p, l, change)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	err := op.call(ctx, p, l, i)
	var contradicted *contradiction
	if errors.As(err, &contradicted) {
		t.Fatalf("%s reported the contradiction: %v", op.name, err)
	}
	settled := phase.phase == registry.Stopped || phase.phase == registry.Cancelled
	if settled && err != nil && op.name != opAbortPreparation && (op.name != opStop || phase.phase == registry.Stopped) {
		t.Fatalf("%s of a %s visit: %v", op.name, phase.phase, err)
	}
	if op.name == opAbortPreparation && err == nil {
		if r := registration(t, p, l); r.Phase != registry.Cancelled && r.Phase != registry.Stopped {
			t.Fatalf("aborted preparation left %s", r.Phase)
		}
		requireValidRecord(t, p, l)
		return
	}
	o, err := p.Abort(t.Context(), l, i)
	if err != nil || o.State != session.PlacementStopped || o.Stopped == nil {
		t.Fatalf("abort after %s: %+v %v", op.name, o, err)
	}
	requireValidRecord(t, p, l)
	if o, err = p.Inspect(t.Context(), l); err != nil || o.State != session.PlacementStopped {
		t.Fatalf("inspect after the abort: %+v %v", o, err)
	}
}

// A stopped visit whose record lost its quiet marker still replays its stop,
// inspects and aborts as stopped: the registration's receipt proves the
// source was quiet, the writer fenced and the output kept.
func TestStoppedVisitWhoseRecordLostItsQuietMarkerStillSettles(t *testing.T) {
	t.Parallel()
	p, _, _, l, data := setup(t)
	i := activeVisit(t, p, l, data)
	if _, err := p.Stop(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	if r := registration(t, p, l); r.Phase != registry.Stopped || r.Receipt == nil || r.Receipt.CheckpointDigest == "" {
		t.Fatalf("stop did not complete: %+v", r)
	}
	contradict(t, p, l, func(r *retained) { r.NativeQuiet = false })
	if _, err := p.Stop(t.Context(), l, i); err != nil {
		t.Errorf("replayed stop of a stopped visit: %v", err)
	}
	if o, err := p.Inspect(t.Context(), l); err != nil || o.State != session.PlacementStopped {
		t.Errorf("inspect of a stopped visit: %+v %v", o.State, err)
	}
	if o, err := p.Abort(t.Context(), l, i); err != nil || o.State != session.PlacementStopped {
		t.Errorf("abort of a stopped visit: %+v %v", o.State, err)
	}
}

// A cancelled registration with a receipt was sealed only once its writer
// was seen fenced, so it proves the fence. An abort of the preparation on
// an executor that lost the visit's record finishes, though the Pod is gone,
// where it used to report the fence unknown for good.
func TestAbortPreparationTrustsTheFenceACancellationReceiptProves(t *testing.T) {
	t.Parallel()
	p, kube, _, l, data := setup(t)
	i := activeVisit(t, p, l, data)
	if _, err := p.Abort(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	r := registration(t, p, l)
	if r.Phase != registry.Cancelled || r.Receipt == nil || !unspent(r.Receipt) {
		t.Fatalf("abort: %+v", r)
	}
	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: p.config.AgentNamespace, Name: runtimekube.RunName(types.UID(r.RunUID))}, &corev1.Pod{}); err == nil {
		t.Fatal("the fenced Pod is still present")
	}
	if err := os.Remove(filepath.Join(p.config.Directory, l.AllocationID+recordSuffix)); err != nil {
		t.Fatal(err)
	}
	if err := p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatalf("abort of the preparation of a cancelled visit: %v", err)
	}
	if local := requireValidRecord(t, p, l); !local.WriterFenced || !local.PreparationAborted {
		t.Fatalf("aborted preparation retained %+v", local)
	}
}

// A repeated abort of a preparation whose tombstoned record was contradicted
// since saves it corrected: the tombstone is already there, so nothing else
// in the abort writes the record.
func TestRepeatedAbortPreparationSavesTheCorrectedRecord(t *testing.T) {
	t.Parallel()
	p, _, _, l, data := setup(t)
	if _, err := p.Prepare(t.Context(), l, data); err != nil {
		t.Fatal(err)
	}
	if err := p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatal(err)
	}
	contradict(t, p, l, func(r *retained) { r.Turnless = true })
	if err := p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatalf("repeated abort of a contradicted preparation: %v", err)
	}
	if local := requireValidRecord(t, p, l); !local.PreparationAborted || local.Turnless {
		t.Fatalf("repeated abort retained %+v", local)
	}
}

// The exit sweep reads an active visit every few seconds, and Inspect and
// Exited write nothing, so a contradiction is logged once per process, not
// once per read. Refusing work logs it the same once.
func TestAContradictionIsLoggedOnce(t *testing.T) {
	t.Parallel()
	p, _, _, l, i, log := activeContradicted(t, func(r *retained) { r.WriterFenced = true })
	for range 3 {
		if _, err := p.Exited(t.Context(), l, i); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Inspect(t.Context(), l); err != nil {
			t.Fatal(err)
		}
		var contradicted *contradiction
		if err := p.Attach(t.Context(), l, i, nil, nil); !errors.As(err, &contradicted) {
			t.Fatalf("attach: %v", err)
		}
	}
	if lines := strings.Count(log.String(), "records the writer fenced, but its session registration is still active"); lines != 1 {
		t.Fatalf("logged the contradiction %d times:\n%s", lines, log.String())
	}
}

// Over the profile protocol a contradicted record is the permanent conflict,
// not the unavailable a retry may cure, and it is
// session.ErrRecordContradicted in process.
func TestAContradictionIsAConflictOverTheProfileProtocol(t *testing.T) {
	t.Parallel()
	p, _, _, l, data := setup(t)
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	contradict(t, p, l, func(r *retained) { r.NativeQuiet = true })
	if err = p.Activate(t.Context(), l, i); !errors.Is(err, session.ErrRecordContradicted) {
		t.Fatalf("activate in process: %v", err)
	}
	c := serveProfile(t, p, l, 10*time.Second)
	err = c.Activate(t.Context(), l, i)
	var remote *profile.RemoteError
	if !errors.As(err, &remote) || remote.Code != profile.CodeConflict || remote.Status != http.StatusConflict {
		t.Fatalf("activate over the protocol: %v", err)
	}
}

// The coordinator's recovery of a preparation whose record is contradicted
// fails, and the session is still cancelled and removed.
func TestCoordinatorCancelsAndRemovesAContradictedPreparation(t *testing.T) {
	t.Parallel()
	p, _, _, l, data := setupKind(t, harness.NativeCodex)
	l.Epoch = 1
	coordinator, err := session.Open(filepath.Join(t.TempDir(), "sessions"), map[string]session.Placement{l.Placement: p}, fixtureMax)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	rec, err := coordinator.Create(l, data, "")
	if err != nil {
		t.Fatal(err)
	}
	launch := *rec.Operations[0].Launch
	if _, err = p.Prepare(t.Context(), launch, data); err != nil {
		t.Fatal(err)
	}
	contradict(t, p, launch, func(r *retained) { r.NativeQuiet = true })
	if _, err = coordinator.Recover(t.Context(), l.ExecutionID); err == nil {
		t.Fatal("recover of a contradicted preparation succeeded")
	}
	if _, err = coordinator.Cancel(t.Context(), l.ExecutionID, uuid.NewString(), nil); err != nil {
		t.Fatalf("cancel of a contradicted preparation: %v", err)
	}
	if err = coordinator.Remove([]string{l.ExecutionID}, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("remove: %v", err)
	}
}

// Attach refuses a contradicted record, and a stop then settles it.
func TestAttachRefusesAContradictedRecordAndStopSettlesIt(t *testing.T) {
	t.Parallel()
	p, _, _, l, data := setup(t)
	i := activeVisit(t, p, l, data)
	contradict(t, p, l, func(r *retained) { r.WriterFenced = true })
	var contradicted *contradiction
	if err := p.Attach(t.Context(), l, i, nil, nil); !errors.As(err, &contradicted) {
		t.Fatalf("attach: %v", err)
	}
	if _, err := p.Stop(t.Context(), l, i); err != nil {
		t.Fatalf("stop: %v", err)
	}
	requireValidRecord(t, p, l)
}
