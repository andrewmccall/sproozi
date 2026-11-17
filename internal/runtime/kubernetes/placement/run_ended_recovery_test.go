package placement

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// startCoordinator opens the real coordinator with p as its one placement
// and retains l's first visit, not yet started.
func startCoordinator(t *testing.T, p *Placement, l session.Launch, input []byte) *session.Coordinator {
	t.Helper()
	coordinator, err := session.Open(filepath.Join(t.TempDir(), "sessions"), map[string]session.Placement{l.Placement: p}, fixtureMax)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err = coordinator.Create(l, input, ""); err != nil {
		t.Fatal(err)
	}
	return coordinator
}

// recoverUntilIdle recovers the execution up to three times, each bounded,
// and reports the last phase and error.
func recoverUntilIdle(t *testing.T, coordinator *session.Coordinator, id string) (session.Phase, error) {
	t.Helper()
	var r session.Record
	var err error
	for range 3 {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		r, err = coordinator.Recover(ctx, id)
		cancel()
		if r.Phase == session.PhaseIdle {
			break
		}
	}
	return r.Phase, err
}

// A refused start whose first end could not be proven, because the
// controller had not yet ended the cancelled run within the abort's bound,
// is ended by the next recover: the launch's tombstone makes Inspect report
// it still preparing and Prepare refuse it again, so the coordinator
// replays AbortPreparation, which now proves the end. Before, Prepare
// answered conflict for the tombstone and recover never ended it.
func TestRecoverEndsARefusedStartWhoseFirstEndWasUnproven(t *testing.T) {
	t.Parallel()
	p, kube, e, l := waitingTask(t)
	p.abortWait = 300 * time.Millisecond
	l.Epoch = 1
	coordinator := startCoordinator(t, p, l, e.initial)
	w := inBackground(t, kube, func() error {
		_, err := coordinator.Recover(t.Context(), l.ExecutionID)
		return err
	})
	run := agentRun(t, kube, l)
	run.Spec.Cancel = true
	if err := kube.Update(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if err := w.ended(t); !errors.Is(err, session.ErrEndUnproven) || !errors.Is(err, session.ErrStartRefused) {
		t.Fatalf("first end of the refused start: %v, want unproven", err)
	}
	if local, err := p.load(l); err != nil || !local.PreparationAborted {
		t.Fatalf("the unproven end left no tombstone: %+v %v", local, err)
	}
	// Its registration is cancelled, but the run has not ended: a start whose
	// end is not yet shown.
	if o, err := p.Inspect(t.Context(), l); err != nil || o.State != session.PlacementPreparing {
		t.Fatalf("Inspect of the unproven aborted preparation: %+v %v", o.State, err)
	}
	setRun(t, kube, l, api.AgentRunPhaseCancelled, "CancelRequested", "spec.cancel was set to true")
	creates := kube.runCreates
	phase, err := recoverUntilIdle(t, coordinator, l.ExecutionID)
	var refused *session.StartRefused
	if phase != session.PhaseIdle || !errors.Is(err, session.ErrStartEnded) || !errors.As(err, &refused) ||
		refused.Reason != "AgentRun "+runName(l)+" was cancelled before its sandbox started: CancelRequested: spec.cancel was set to true" {
		t.Fatalf("recover of the unproven refused start: %s %v", phase, err)
	}
	if kube.runCreates != creates {
		t.Fatal("recover created a run for an aborted preparation")
	}
	// Settled, but Inspect claims no more than preparing (ADR 0067).
	if o, err := p.Inspect(t.Context(), l); err != nil || o.State != session.PlacementPreparing {
		t.Fatalf("Inspect of the settled aborted preparation: %+v %v", o.State, err)
	}
}

// The same over the placement protocol, as a home coordinates a session it
// moved to the cluster: the first end is unproven, and the next recover
// gets refused (409) for the tombstoned launch, replays abort-preparation
// and ends the start.
func TestHomeRecoverEndsARefusedStartWhoseFirstEndWasUnproven(t *testing.T) {
	t.Parallel()
	p, kube, _, l, data := setup(t)
	kube.queued = true
	p.abortWait = 300 * time.Millisecond
	l.Epoch = 1
	home := serveProfile(t, p, l, time.Minute)
	coordinator, err := session.Open(filepath.Join(t.TempDir(), "sessions"), map[string]session.Placement{l.Placement: home}, fixtureMax)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	if _, err = coordinator.Create(l, data, ""); err != nil {
		t.Fatal(err)
	}
	w := inBackground(t, kube, func() error {
		_, err := coordinator.Recover(t.Context(), l.ExecutionID)
		return err
	})
	run := agentRun(t, kube, l)
	run.Spec.Cancel = true
	if err = kube.Update(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if err = w.ended(t); !errors.Is(err, session.ErrEndUnproven) || !errors.Is(err, session.ErrStartRefused) {
		t.Fatalf("first end of the remote refused start: %v, want unproven", err)
	}
	setRun(t, kube, l, api.AgentRunPhaseCancelled, "CancelRequested", "spec.cancel was set to true")
	phase, err := recoverUntilIdle(t, coordinator, l.ExecutionID)
	var refused *session.StartRefused
	if phase != session.PhaseIdle || !errors.Is(err, session.ErrStartEnded) || !errors.As(err, &refused) ||
		!strings.Contains(refused.Reason, "was cancelled before its sandbox started: CancelRequested") {
		t.Fatalf("remote recover of the unproven refused start: %s %v", phase, err)
	}
}

// A start whose run failed and was then deleted, as its retention TTL does,
// while no Prepare was waiting, is refused by the next recover without a
// second AgentRun at the same name: a registration that bound a run never
// gets another. Before, ensureRun created a new run, BindRun refused it and
// every recover answered conflict.
func TestRecoverRefusesAStartWhoseRunWasDeletedWithoutCreatingAnother(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		replace bool
		want    string
	}{
		{name: "deleted", want: " was deleted before its sandbox started"},
		{name: "replaced", replace: true, want: " was replaced before its sandbox started"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p, kube, e, l := waitingTask(t)
			l.Epoch = 1
			coordinator := startCoordinator(t, p, l, e.initial)
			ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
			defer cancel()
			if _, err := coordinator.Recover(ctx, l.ExecutionID); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("queued start: %v", err)
			}
			old := agentRun(t, kube, l)
			setRun(t, kube, l, api.AgentRunPhaseFailed, "RuntimeInvalid", "image is required")
			if err := kube.Client.Delete(t.Context(), agentRun(t, kube, l)); err != nil {
				t.Fatal(err)
			}
			if test.replace {
				replacement := &api.AgentRun{ObjectMeta: old.ObjectMeta, Spec: old.Spec}
				replacement.ResourceVersion, replacement.UID = "", replacementUID
				if err := kube.Client.Create(t.Context(), replacement); err != nil {
					t.Fatal(err)
				}
				// The replacement ended too: a refusal must still say the
				// bound run was replaced, not how the replacement ended.
				setRun(t, kube, l, api.AgentRunPhaseSucceeded, "ExecutionSucceeded", "the replacement's own ending")
			}
			creates := kube.runCreates
			r, err := coordinator.Recover(t.Context(), l.ExecutionID)
			var refused *session.StartRefused
			if !errors.As(err, &refused) || !strings.HasPrefix(refused.Reason, "AgentRun "+runName(l)+test.want) {
				t.Fatalf("recover of a start whose run was %s: %s %v", test.name, r.Phase, err)
			}
			// A deleted run's preparation is fenced and the start ends. A
			// replaced run cannot be shown to hold no Pod of the bound one,
			// so its end stays unproven, as ADR 0060 records for it. A
			// recover, which finds the launch's tombstone, still says it was
			// replaced, and why the end is unproven, alone.
			phase := r.Phase
			if phase != session.PhaseIdle {
				phase, err = recoverUntilIdle(t, coordinator, l.ExecutionID)
			}
			var unproven session.UnprovenEnd
			if test.replace && (!errors.As(err, &refused) ||
				!strings.HasPrefix(refused.Reason, "AgentRun "+runName(l)+" was replaced before its sandbox started: it now has UID "+replacementUID) ||
				!errors.As(err, &unproven) || !strings.HasPrefix(unproven.UnprovenEnd(), "AgentRun "+runName(l)+" now has UID "+replacementUID) ||
				!strings.Contains(unproven.UnprovenEnd(), "an administrator replaced it") ||
				strings.Contains(unproven.UnprovenEnd(), "session profile")) {
				t.Fatalf("recover of the replaced run's tombstoned start: %v", err)
			}
			if !test.replace && (phase != session.PhaseIdle || !errors.Is(err, session.ErrStartEnded)) ||
				test.replace && (phase != session.PhasePreparing || !errors.Is(err, session.ErrEndUnproven)) {
				t.Fatalf("recover of a start whose run was %s: %s %v", test.name, phase, err)
			}
			var after api.AgentRun
			err = kube.Get(t.Context(), client.ObjectKey{Namespace: p.config.Namespace, Name: old.Name}, &after)
			switch {
			case kube.runCreates != creates:
				t.Fatalf("recover created %d AgentRuns", kube.runCreates-creates)
			case !test.replace && err == nil:
				t.Fatalf("an AgentRun %s exists again with UID %s (old %s)", after.Name, after.UID, old.UID)
			case test.replace && (err != nil || after.UID != replacementUID):
				t.Fatalf("the replacement run changed: %+v %v", after.ObjectMeta, err)
			}
		})
	}
}

// A registration an administrator deleted while the start waited refuses
// it, and its end stays unproven: nothing is left to fence its Pod by. The
// error states that cause alone, what an administrator must confirm, and
// later recovers, which find the tombstone, say the same.
func TestRecoverOfAStartWhoseRegistrationWasDeletedNamesTheCheck(t *testing.T) {
	t.Parallel()
	p, kube, e, l := waitingTask(t)
	l.Epoch = 1
	coordinator := startCoordinator(t, p, l, e.initial)
	w := inBackground(t, kube, func() error {
		_, err := coordinator.Recover(t.Context(), l.ExecutionID)
		return err
	})
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: p.config.Namespace, Name: registry.NameForRun(runName(l))}}
	if err := kube.Client.Delete(t.Context(), cm); err != nil {
		t.Fatal(err)
	}
	check := func(err error) {
		t.Helper()
		var refused *session.StartRefused
		var unproven session.UnprovenEnd
		if !errors.Is(err, session.ErrEndUnproven) || !errors.As(err, &refused) ||
			refused.Reason != "the session registration for AgentRun "+runName(l)+" was deleted before its sandbox started" ||
			!errors.As(err, &unproven) || !strings.Contains(unproven.UnprovenEnd(), "was deleted, so its Pod cannot be fenced; an administrator must confirm") ||
			strings.Contains(unproven.UnprovenEnd(), "session profile") {
			t.Fatalf("refused start whose registration was deleted: %v", err)
		}
	}
	check(w.ended(t))
	_, err := coordinator.Recover(t.Context(), l.ExecutionID)
	check(err)
}

// The check before ensureRun reads the bound run; when that read fails it
// is no evidence, and the start goes on. Even then a registration that bound
// a run never gets another: ensureRun only reads it, so a run deleted
// meanwhile is not created again.
func TestPrepareNeverCreatesASecondRunWhenItsCheckCannotReadTheFirst(t *testing.T) {
	t.Parallel()
	p, kube, e, l := waitingTask(t)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if _, err := p.Prepare(ctx, l, e.initial); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued preparation: %v", err)
	}
	if err := kube.Client.Delete(t.Context(), agentRun(t, kube, l)); err != nil {
		t.Fatal(err)
	}
	var failed atomic.Bool
	fail := func(obj client.Object) error {
		if _, ok := obj.(*api.AgentRun); ok && failed.CompareAndSwap(false, true) {
			return apierrors.NewServiceUnavailable("the API server cannot answer")
		}
		return nil
	}
	kube.getErr.Store(&fail)
	creates := kube.runCreates
	ctx, cancel = context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	_, err := p.Prepare(ctx, l, e.initial)
	if !failed.Load() || err == nil || kube.runCreates != creates {
		t.Fatalf("prepare after a failed check of a deleted run: %v, runs created %d", err, kube.runCreates-creates)
	}
	var run api.AgentRun
	if err = kube.Client.Get(t.Context(), client.ObjectKey{Namespace: p.config.Namespace, Name: runName(l)}, &run); !apierrors.IsNotFound(err) {
		t.Fatalf("an AgentRun exists again at the bound name: %+v %v", run.ObjectMeta, err)
	}
}
