package placement

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/session"
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
			}
			creates := kube.runCreates
			r, err := coordinator.Recover(t.Context(), l.ExecutionID)
			var refused *session.StartRefused
			if !errors.As(err, &refused) || !strings.HasPrefix(refused.Reason, "AgentRun "+runName(l)+test.want) {
				t.Fatalf("recover of a start whose run was %s: %s %v", test.name, r.Phase, err)
			}
			// A deleted run's preparation is fenced and the start ends. A
			// replaced run cannot be shown to hold no Pod of the bound one,
			// so its end stays unproven, as ADR 0060 records for it, and a
			// recover, which finds the launch's tombstone, says only that
			// its preparation was aborted.
			phase := r.Phase
			if phase != session.PhaseIdle {
				phase, err = recoverUntilIdle(t, coordinator, l.ExecutionID)
			}
			if test.replace && (!errors.As(err, &refused) ||
				refused.Reason != "the preparation of AgentRun "+runName(l)+" was aborted before its sandbox started") {
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
