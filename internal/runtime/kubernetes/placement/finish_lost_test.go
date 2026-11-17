package placement

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	runtimekube "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// errCutStream is an exec whose stream the helper's own end cut, the
// failure a lost finish acknowledgement shows. Its text stands for whatever
// the exec reported, which the placement keeps.
var errCutStream = errors.New("fixture exec stream reset")

// helperContainer is the index of the capture helper's status in an
// admitted Pod.
const helperContainer = 1

// changeHelper is a finish that changes the helper's reported state with
// change and whose exec then reports lost.
func changeHelper(kube client.Client, change func(*corev1.ContainerStatus), lost error) func(context.Context, string, string) error {
	return func(ctx context.Context, namespace, pod string) error {
		var actual corev1.Pod
		if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: pod}, &actual); err != nil {
			return err
		}
		change(&actual.Status.ContainerStatuses[helperContainer])
		if err := kube.Status().Update(ctx, &actual); err != nil {
			return err
		}
		return lost
	}
}

// helperEnds is a finish whose helper ended with code before its exec
// reported lost.
func helperEnds(kube client.Client, code int32, lost error) func(context.Context, string, string) error {
	return changeHelper(kube, func(s *corev1.ContainerStatus) { s.State = fixtureTermination(s.ContainerID, code) }, lost)
}

// fenceHeld reports whether the visit's Pod still carries the writer-fence
// finalizer, which a stop releases only after its finish.
func fenceHeld(t *testing.T, p *Placement, l session.Launch) bool {
	t.Helper()
	_, r, err := p.retained(t.Context(), l)
	if err != nil {
		t.Fatal(err)
	}
	pod, err := p.exactPod(t.Context(), r)
	if apierrors.IsNotFound(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return slices.Contains(pod.Finalizers, runtimekube.ManagedWriterFenceFinalizer)
}

// A stop whose writer was fenced, whose usage was sealed and whose
// checkpoint was kept does not stay unproven because the acknowledgement of
// its last fixed command was lost: a fresh read of the exact Pod showing the
// helper ended with code 0, which it does only once it acknowledged a
// finish, proves the finish. The stop then completes as an acknowledged one
// does, with one outcome on every replay, and the lost exec's error is
// logged. Guards: the proof in Placement.finish, and the kept exec error.
func TestALostFinishOfAHelperThatEndedCompletesTheStop(t *testing.T) {
	t.Parallel()
	for _, lost := range []error{errCutStream, exitStatus(137)} {
		t.Run(lost.Error(), func(t *testing.T) {
			t.Parallel()
			p, kube, e, l, data := setup(t)
			var log bytes.Buffer
			p.config.Log = &log
			i := activeVisit(t, p, l, data)
			e.finishOutcome = helperEnds(kube, 0, lost)
			s, err := p.Stop(t.Context(), l, i)
			if err != nil {
				t.Fatalf("a stop whose helper ended 0 stayed unproven: %v", err)
			}
			if !s.ActualStopped || s.Receipt == nil || s.CheckpointDigest != e.meta.SHA256 || e.finishes != 1 {
				t.Fatalf("stopped %+v after %d finishes", s, e.finishes)
			}
			if fenceHeld(t, p, l) {
				t.Fatal("the writer-fence finalizer was kept after a proven finish")
			}
			if !strings.Contains(log.String(), lost.Error()) || !strings.Contains(log.String(), "finish") {
				t.Fatalf("the lost finish was not logged with its exec error: %q", log.String())
			}
			// One outcome: the replay and Inspect report the same stop, and
			// nothing runs finish again.
			replay, err := p.Stop(t.Context(), l, i)
			if err != nil || replay.CheckpointDigest != s.CheckpointDigest || !replay.ActualStopped ||
				replay.Receipt == nil || len(replay.Receipt.Usage) != len(s.Receipt.Usage) {
				t.Fatalf("replay %+v: %v", replay, err)
			}
			o, err := p.Inspect(t.Context(), l)
			if err != nil || o.State != session.PlacementStopped || o.Stopped == nil || o.Stopped.CheckpointDigest != s.CheckpointDigest {
				t.Fatalf("inspect %+v: %v", o, err)
			}
			if e.finishes != 1 {
				t.Fatalf("finish ran %d times", e.finishes)
			}
		})
	}
}

// Nothing else proves a failed finish: a helper still running, one that
// ended with another code, a termination the runtime did not report for
// that container, or a Pod that cannot be read leaves the stop unproven
// exactly as before, with the Pod's fence held, and the exec's error kept
// in the error. Guards: each condition of the proof.
func TestALostFinishStaysUnprovenWithoutProof(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		lost    error
		outcome func(kube *admissionClient, lost error) func(context.Context, string, string) error
	}{
		{"helper running", errCutStream, func(_ *admissionClient, lost error) func(context.Context, string, string) error {
			return func(context.Context, string, string) error { return lost }
		}},
		{"helper ended 137", errCutStream, func(kube *admissionClient, lost error) func(context.Context, string, string) error {
			return helperEnds(kube, 137, lost)
		}},
		{"helper ended 1", exitStatus(1), func(kube *admissionClient, lost error) func(context.Context, string, string) error {
			return helperEnds(kube, 1, lost)
		}},
		{"termination of another container", errCutStream, func(kube *admissionClient, lost error) func(context.Context, string, string) error {
			return changeHelper(kube, func(s *corev1.ContainerStatus) {
				s.State = fixtureTermination("containerd://replacement", 0)
			}, lost)
		}},
		{"termination the kubelet made up", errCutStream, func(kube *admissionClient, lost error) func(context.Context, string, string) error {
			return changeHelper(kube, func(s *corev1.ContainerStatus) {
				s.State = fixtureTermination(s.ContainerID, 0)
				s.State.Terminated.Reason = containerStatusUnknown
			}, lost)
		}},
		{"Pod unreadable", errCutStream, func(kube *admissionClient, lost error) func(context.Context, string, string) error {
			end := helperEnds(kube, 0, lost)
			return func(ctx context.Context, namespace, pod string) error {
				err := end(ctx, namespace, pod)
				fail := func(obj client.Object) error {
					if _, ok := obj.(*corev1.Pod); ok {
						return errors.New("fixture API server unavailable")
					}
					return nil
				}
				kube.getErr.Store(&fail)
				return err
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p, kube, e, l, data := setup(t)
			i := activeVisit(t, p, l, data)
			e.finishOutcome = test.outcome(kube, test.lost)
			s, err := p.Stop(t.Context(), l, i)
			kube.getErr.Store(nil)
			var failed *commandFailed
			if err == nil || s.ActualStopped || s.Receipt != nil || !errors.As(err, &failed) {
				t.Fatalf("an unproven finish was accepted: %+v %v", s, err)
			}
			if !strings.Contains(err.Error(), "placement: fixed command unavailable") || !strings.Contains(err.Error(), test.lost.Error()) {
				t.Fatalf("the exec error was not kept: %v", err)
			}
			if !fenceHeld(t, p, l) {
				t.Fatal("the writer-fence finalizer was released without a proven finish")
			}
			if _, r, err := p.retained(t.Context(), l); err != nil || r.Phase != registry.Stopped {
				t.Fatalf("registration %v: %v", r.Phase, err)
			}
		})
	}
}

// The proof holds only beside a stopped registration and a fenced writer:
// before the stop completed, a helper that ended 0 proves nothing about the
// stop. Guards: the registration and fence conditions of helperFinished.
func TestAnEndedHelperProvesNoFinishBeforeTheStopCompleted(t *testing.T) {
	t.Parallel()
	p, kube, _, l, data := setup(t)
	activeVisit(t, p, l, data)
	local, r, err := p.retained(t.Context(), l)
	if err != nil {
		t.Fatal(err)
	}
	pod, err := p.exactPod(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	if err = helperEnds(kube, 0, nil)(t.Context(), pod.Namespace, pod.Name); err != nil {
		t.Fatal(err)
	}
	if proof, ok := p.helperFinished(t.Context(), local, r); ok {
		t.Fatalf("an active registration's helper proved a finish: %s", proof)
	}
	stopped := r
	stopped.Phase, stopped.Receipt = registry.Stopped, &registry.StopReceipt{}
	if proof, ok := p.helperFinished(t.Context(), local, stopped); ok {
		t.Fatalf("an unfenced writer's helper proved a finish: %s", proof)
	}
	local.WriterFenced = true
	if _, ok := p.helperFinished(t.Context(), local, stopped); !ok {
		t.Fatal("a stopped registration's helper that ended 0 proved nothing")
	}
}

// A Pod gone after a failed finish is proof, as the replay of a stopped
// visit already takes it (finishStopped): the registration is stopped, the
// writer's fence is retained, and no helper is left to finish. Guards: that
// the stop's own end and its replay agree.
func TestALostFinishOfAPodThatIsGoneCompletesTheStop(t *testing.T) {
	t.Parallel()
	p, kube, e, l, data := setup(t)
	i := activeVisit(t, p, l, data)
	e.finishOutcome = func(ctx context.Context, namespace, name string) error {
		var pod corev1.Pod
		if err := kube.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &pod); err != nil {
			return err
		}
		pod.Finalizers = nil
		if err := kube.Update(ctx, &pod); err != nil {
			return err
		}
		if err := kube.Client.Delete(ctx, &pod); err != nil {
			return err
		}
		return errCutStream
	}
	s, err := p.Stop(t.Context(), l, i)
	if err != nil || !s.ActualStopped || s.Receipt == nil || s.CheckpointDigest != e.meta.SHA256 {
		t.Fatalf("stop of a Pod gone after its finish: %+v %v", s, err)
	}
	if replay, err := p.Stop(t.Context(), l, i); err != nil || replay.CheckpointDigest != s.CheckpointDigest {
		t.Fatalf("replay %+v: %v", replay, err)
	}
}

// A stop left unproven by a lost finish with the helper still running is
// retried: Stop and Abort of the stopped registration run finish again, and
// a lost acknowledgement there is proven the same way. Guards: the replay
// path (finishStopped) uses the same proof.
func TestARetriedStopProvesALostFinishTheSameWay(t *testing.T) {
	t.Parallel()
	for _, abort := range []bool{false, true} {
		t.Run(map[bool]string{false: "retried stop", true: "abort of the stopped visit"}[abort], func(t *testing.T) {
			t.Parallel()
			p, kube, e, l, data := setup(t)
			i := activeVisit(t, p, l, data)
			e.finishOutcome = func(context.Context, string, string) error { return errCutStream }
			if _, err := p.Stop(t.Context(), l, i); err == nil {
				t.Fatal("a lost finish with the helper running was accepted")
			}
			e.finishOutcome = helperEnds(kube, 0, errCutStream)
			var digest string
			if abort {
				o, err := p.Abort(t.Context(), l, i)
				if err != nil || o.State != session.PlacementStopped || o.Stopped == nil {
					t.Fatalf("abort of the stopped visit: %+v %v", o, err)
				}
				digest = o.Stopped.CheckpointDigest
			} else {
				s, err := p.Stop(t.Context(), l, i)
				if err != nil || !s.ActualStopped {
					t.Fatalf("retried stop: %+v %v", s, err)
				}
				digest = s.CheckpointDigest
			}
			if digest != e.meta.SHA256 || e.finishes != 2 || fenceHeld(t, p, l) {
				t.Fatalf("digest %q after %d finishes, fence held %v", digest, e.finishes, fenceHeld(t, p, l))
			}
		})
	}
}

// Through the coordinator, a stop whose finish acknowledgement was lost and
// whose helper ended 0 ends the execution idle with its checkpoint, where it
// ended unknown-stop before; one whose helper still runs ends unknown-stop
// as before, and recovering it then completes it. Guards: the session's
// phase after a lost finish, which the remote executor's driver and the
// exit sweep act on.
func TestCoordinatorStopWithALostFinish(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name  string
		ended bool
		want  session.Phase
	}{
		{"helper ended 0", true, session.PhaseIdle},
		{"helper running", false, session.PhaseUnknownStop},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p, kube, e, l, data := setup(t)
			l.Epoch = 1
			coordinator, err := session.Open(filepath.Join(t.TempDir(), "sessions"), map[string]session.Placement{l.Placement: p}, fixtureMax)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = coordinator.Close() })
			if _, err = coordinator.Create(l, data, ""); err != nil {
				t.Fatal(err)
			}
			if _, err = coordinator.Recover(t.Context(), l.ExecutionID); err != nil {
				t.Fatal(err)
			}
			e.finishOutcome = func(context.Context, string, string) error { return errCutStream }
			if test.ended {
				e.finishOutcome = helperEnds(kube, 0, errCutStream)
			}
			r, err := coordinator.Finalize(t.Context(), l.ExecutionID, uuid.NewString())
			if r.Phase != test.want {
				t.Fatalf("finalize ended %s, want %s: %v", r.Phase, test.want, err)
			}
			if test.ended {
				if err != nil || len(r.Visits) != 1 || !r.Visits[0].ActualStopped || r.Visits[0].Receipt == nil ||
					r.Checkpoint.Metadata.SHA256 != e.meta.SHA256 {
					t.Fatalf("finalized %+v: %v", r, err)
				}
				return
			}
			e.finishOutcome = nil
			if r, err = coordinator.Recover(t.Context(), l.ExecutionID); err != nil || r.Phase != session.PhaseIdle {
				t.Fatalf("recover after an unproven finish: %s %v", r.Phase, err)
			}
		})
	}
}
