package placement

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// contradict rewrites l's retained record as a corrupt or foreign write
// would, with change applied and without validation.
func contradict(t *testing.T, p *Placement, l session.Launch, change func(*retained)) {
	t.Helper()
	path := filepath.Join(p.config.Directory, l.AllocationID+recordSuffix)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r retained
	if err = json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	change(&r)
	if data, err = json.Marshal(r); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// requireValidRecord fails unless l's retained record is valid beside its
// registration.
func requireValidRecord(t *testing.T, p *Placement, l session.Launch) retained {
	t.Helper()
	local, err := p.load(l)
	if err != nil {
		t.Fatalf("retained record after the operation: %v", err)
	}
	if _, err = local.check(registrationOf(t, p, local)); err != nil {
		t.Fatalf("retained record after the operation: %v", err)
	}
	return local
}

func activeContradicted(t *testing.T, change func(*retained)) (*Placement, *admissionClient, *protocolExecutor, session.Launch, session.Incarnation, *bytes.Buffer) {
	t.Helper()
	p, kube, e, l, data := setup(t)
	log := &bytes.Buffer{}
	p.config.Log = log
	i := activeVisit(t, p, l, data)
	contradict(t, p, l, change)
	return p, kube, e, l, i, log
}

// A contradicted record gives no new authority: activation and a repeated
// preparation refuse it, naming the contradiction. Aborting the
// preparation, which ends it, still works and leaves a valid record.
func TestContradictedRecordCannotStartWork(t *testing.T) {
	t.Parallel()
	p, _, _, l, data := setup(t)
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	contradict(t, p, l, func(r *retained) { r.NativeQuiet = true })
	var contradicted *contradiction
	err = p.Activate(t.Context(), l, i)
	if !errors.As(err, &contradicted) || !strings.Contains(err.Error(), "records a stop, but its session registration was never active") {
		t.Fatalf("activated a contradicted record: %v", err)
	}
	if _, err = p.Prepare(t.Context(), l, data); !errors.As(err, &contradicted) {
		t.Fatalf("prepared a contradicted record again: %v", err)
	}
	if r := registration(t, p, l); r.Phase != registry.Prepared {
		t.Fatalf("refused activation changed the registration: %+v", r)
	}
	if err = p.AbortPreparation(t.Context(), l); err != nil {
		t.Fatalf("abort of a contradicted preparation: %v", err)
	}
	if r := registration(t, p, l); r.Phase != registry.Cancelled {
		t.Fatalf("aborted preparation: %+v", r)
	}
	if local := requireValidRecord(t, p, l); local.NativeQuiet || !local.PreparationAborted {
		t.Fatalf("aborted preparation kept %+v", local)
	}
}

// Cancelling a visit whose record claims a fenced writer and a kept
// checkpoint while its registration is still active forgets both, fences
// the writer it observes and leaves a valid record. Inspect and the exit
// sweep read the same record without failing.
func TestCancelEndsAVisitWhoseRecordContradictsItsRegistration(t *testing.T) {
	t.Parallel()
	p, _, _, l, i, log := activeContradicted(t, func(r *retained) {
		r.WriterFenced, r.OutputDigest = true, recordedDigest
	})
	if o, err := p.Inspect(t.Context(), l); err != nil || o.State != session.PlacementActive {
		t.Fatalf("inspect of a contradicted active visit: %+v %v", o, err)
	}
	if exited, err := p.Exited(t.Context(), l, i); err != nil || exited {
		t.Fatalf("exit sweep of a contradicted active visit: %v %v", exited, err)
	}
	o, err := p.Abort(t.Context(), l, i)
	if err != nil || o.State != session.PlacementStopped || o.Stopped == nil || o.Stopped.Receipt == nil {
		t.Fatalf("cancel of a contradicted visit: %+v %v", o, err)
	}
	if local := requireValidRecord(t, p, l); !local.WriterFenced || local.OutputDigest != "" {
		t.Fatalf("cancelled visit retained %+v", local)
	}
	if !strings.Contains(log.String(), "records the writer fenced, but its session registration is still active") {
		t.Fatalf("contradiction not logged: %q", log.String())
	}
}

// A fence the record claimed against its registration is forgotten, not
// trusted: with the Pod gone, nothing proves the writer stopped, so the
// cancellation stays unproven rather than reporting a stop.
func TestCancelDoesNotTrustAContradictedFence(t *testing.T) {
	t.Parallel()
	p, kube, _, l, i, _ := activeContradicted(t, func(r *retained) { r.WriterFenced = true })
	pod, err := p.exactPod(t.Context(), registration(t, p, l))
	if err != nil {
		t.Fatal(err)
	}
	original := pod.DeepCopy()
	pod.Finalizers = nil
	if err = kube.Patch(t.Context(), pod, client.MergeFrom(original)); err != nil {
		t.Fatal(err)
	}
	kube.deferTermination = true
	if err = kube.Delete(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	if err = kube.Get(t.Context(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err == nil {
		t.Fatal("Pod still present")
	}
	o, err := p.Abort(t.Context(), l, i)
	if err == nil || o.State == session.PlacementStopped || o.Stopped != nil {
		t.Fatalf("contradicted fence proved a vanished writer stopped: %+v %v", o, err)
	}
}

// A graceful stop of a visit whose record claims output it never captured
// captures the stopped state rather than keep the claim.
func TestStopCapturesAgainWhatAContradictedRecordClaimed(t *testing.T) {
	t.Parallel()
	p, _, e, l, i, _ := activeContradicted(t, func(r *retained) { r.OutputDigest = recordedDigest })
	s, err := p.Stop(t.Context(), l, i)
	if err != nil || s.CheckpointDigest != e.meta.SHA256 || e.captures != 1 {
		t.Fatalf("stop of a contradicted visit: %+v %v captures %d", s, err, e.captures)
	}
	if local := requireValidRecord(t, p, l); local.OutputDigest != e.meta.SHA256 {
		t.Fatalf("stopped visit retained %+v", local)
	}
}

// The coordinator cancels and then removes a session whose Kubernetes
// visit's record contradicts its registration: a loose record never keeps a
// session from being stopped and removed.
func TestCoordinatorRemovesASessionWhoseRecordContradictsItsRegistration(t *testing.T) {
	t.Parallel()
	p, _, _, l, data := setupKind(t, harness.NativeCodex)
	l.Epoch = 1
	coordinator, err := session.Open(filepath.Join(t.TempDir(), "sessions"), map[string]session.Placement{l.Placement: p}, fixtureMax)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err = coordinator.Create(l, data, ""); err != nil {
		t.Fatal(err)
	}
	r, err := coordinator.Recover(t.Context(), l.ExecutionID)
	if err != nil || r.Active == nil {
		t.Fatalf("recover: %+v %v", r, err)
	}
	contradict(t, p, r.Active.Launch, func(r *retained) { r.WriterFenced, r.Turnless = true, true })
	if r, err = coordinator.Cancel(t.Context(), l.ExecutionID, uuid.NewString(), nil); err != nil || r.Phase != session.PhaseCancelled {
		t.Fatalf("cancel: %+v %v", r, err)
	}
	if err = coordinator.Remove([]string{l.ExecutionID}, func() ([]string, error) { return nil, nil }); err != nil {
		t.Fatalf("remove: %v", err)
	}
}

// Every rule names a contradiction the table records, so none is dead.
func TestEveryRetainedRuleIsRecorded(t *testing.T) {
	t.Parallel()
	recorded, err := os.ReadFile("testdata/retained-records.txt")
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range retainedRules {
		if !bytes.Contains(recorded, []byte(rule.contradiction)) {
			t.Errorf("no recorded record breaks %q", rule.contradiction)
		}
	}
}
