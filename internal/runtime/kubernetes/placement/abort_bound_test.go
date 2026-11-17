package placement

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

// A cancelled visit whose Pod was deleted but whose agent container is never
// seen to stop, as on a lost node where the writer-fence finalizer keeps the
// Pod, does not hold its cancellation for ever, even for a caller with no
// deadline such as the executor's driver. Abort gives up after its bound and
// reports the fence unknown: the coordinator records the cancellation as
// unknown-stop, never as a proven stop, and a recover once the container is
// seen to stop finishes it.
func TestCancelOfAPodNeverSeenToStopIsBoundedAndUnproven(t *testing.T) {
	t.Parallel()
	p, kube, e, l, data := setup(t)
	p.fenceWait = 300 * time.Millisecond
	l = freshLaunch(t, e, l, data, "")
	l.Epoch = 1
	coordinator, err := session.Open(filepath.Join(t.TempDir(), "sessions"), map[string]session.Placement{l.Placement: p}, fixtureMax)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err = coordinator.Create(l, e.initial, ""); err != nil {
		t.Fatal(err)
	}
	if r, err := coordinator.Recover(t.Context(), l.ExecutionID); err != nil || r.Phase != session.PhaseActive {
		t.Fatalf("recover: %+v %v", r.Phase, err)
	}
	kube.deferTermination = true
	type result struct {
		r   session.Record
		err error
	}
	done := make(chan result, 1)
	go func() {
		r, err := coordinator.Cancel(t.Context(), l.ExecutionID, uuid.NewString(), nil)
		done <- result{r, err}
	}()
	var got result
	select {
	case got = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the cancellation still waits for a Pod whose container is never seen to stop")
	}
	if !errors.Is(got.err, errWriterFenceUnknown) || got.r.Phase != session.PhaseUnknownStop {
		t.Fatalf("unproven cancellation: %s %v", got.r.Phase, got.err)
	}
	if local, err := p.load(l); err != nil || local.WriterFenced {
		t.Fatalf("a fence was recorded without a stop: %+v %v", local, err)
	}
	r := registration(t, p, l)
	if r.Receipt != nil {
		t.Fatalf("usage was sealed without a proven stop: %+v", r.Receipt)
	}
	pod, found := sandboxPod(t, kube, r.RunUID)
	if !found {
		t.Fatal("the Pod was removed without a proven stop")
	}
	pod.Status.ContainerStatuses[0].State = fixtureTermination(pod.Status.ContainerStatuses[0].ContainerID, 137)
	if err = kube.Status().Update(t.Context(), pod); err != nil {
		t.Fatal(err)
	}
	if rec, err := coordinator.Recover(t.Context(), l.ExecutionID); err != nil || rec.Phase != session.PhaseCancelled {
		t.Fatalf("recover once the container stopped: %s %v", rec.Phase, err)
	}
}
