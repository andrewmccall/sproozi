package placement

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// waitingTask is a first visit of a task with no deadline (ADR 0056) whose
// run the controller leaves Queued with no Pod.
func waitingTask(t *testing.T) (*Placement, *admissionClient, *protocolExecutor, session.Launch) {
	t.Helper()
	p, kube, e, l, data := setup(t)
	p.config.MaxDuration = 0
	var policy api.AgentPolicy
	if err := kube.Get(t.Context(), client.ObjectKey{Namespace: fixtureNamespace, Name: fixturePolicy}, &policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.MaxExecutionDuration.Duration = 0
	if err := kube.Update(t.Context(), &policy); err != nil {
		t.Fatal(err)
	}
	l = freshLaunch(t, e, l, data, initialFixtureTask)
	l.Deadline = time.Time{}
	kube.queued = true
	return p, kube, e, l
}

// setRun changes the launch's run status as the controller would.
func setRun(t *testing.T, kube *admissionClient, l session.Launch, phase api.AgentRunPhase, reason, message string) {
	t.Helper()
	run := agentRun(t, kube, l)
	run.Status.Phase = phase
	if reason != "" {
		apimeta.SetStatusCondition(&run.Status.Conditions, metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse,
			Reason: reason, Message: message})
	}
	if err := kube.Client.Status().Update(t.Context(), run); err != nil {
		t.Fatal(err)
	}
}

// preparation is a Prepare running in the background, and the reads of its
// run that its wait makes.
type preparation struct {
	result <-chan error
	reads  chan struct{}
}

// prepareInBackground starts Prepare of l and returns once its wait for the
// Pod has read the bound run.
func prepareInBackground(t *testing.T, p *Placement, kube *admissionClient, l session.Launch, input []byte) *preparation {
	t.Helper()
	return inBackground(t, kube, func() error {
		_, err := p.Prepare(t.Context(), l, input)
		return err
	})
}

// inBackground starts prepare, which prepares a launch through kube, and
// returns once its wait for the Pod has read the bound run.
func inBackground(t *testing.T, kube *admissionClient, prepare func() error) *preparation {
	t.Helper()
	reads := make(chan struct{}, 1)
	read := func(*api.AgentRun) {
		select {
		case reads <- struct{}{}:
		default:
		}
	}
	kube.runRead.Store(&read)
	t.Cleanup(func() { kube.runRead.Store(nil) })
	result := make(chan error, 1)
	go func() { result <- prepare() }()
	w := &preparation{result: result, reads: reads}
	w.awaitFreshReads(t, 2)
	return w
}

// awaitFreshReads waits until the wait has read the run n times after this
// call, so the last of them began after anything the caller changed before.
func (w *preparation) awaitFreshReads(t *testing.T, n int) {
	t.Helper()
	select {
	case <-w.reads:
	default:
	}
	timeout := time.After(10 * time.Second)
	for range n {
		select {
		case <-w.reads:
		case err := <-w.result:
			t.Fatalf("Prepare returned while it should wait: %v", err)
		case <-timeout:
			t.Fatal("Prepare's wait stopped reading its run")
		}
	}
}

// ended waits for Prepare to return, which it must do promptly.
func (w *preparation) ended(t *testing.T) error {
	t.Helper()
	select {
	case err := <-w.result:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("Prepare still waits for a Pod its run can never get")
		return nil
	}
}

// requireRefused checks that err is the typed refusal and that its reason
// names each of want.
func requireRefused(t *testing.T, err error, want ...string) {
	t.Helper()
	var refused *session.StartRefused
	if !errors.Is(err, session.ErrStartRefused) || !errors.As(err, &refused) || refused.Ended {
		t.Fatalf("Prepare: %v, want a start refusal", err)
	}
	for _, w := range want {
		if !strings.Contains(refused.Reason, w) {
			t.Errorf("reason %q does not name %q", refused.Reason, w)
		}
	}
}

// A task with no deadline (ADR 0056) whose AgentRun ended before its Pod
// existed, here Failed as on an invalid runtime, must not leave Prepare
// waiting for a Pod that will never come. Before ADR 0056 the execution
// deadline ended this wait; without one, waitPod waited until its caller
// gave up, and the remote executor's driver never does. Prepare refuses it
// at once, naming the run's phase and its Ready condition (ADR 0067).
func TestPrepareOfTaskWithoutDeadlineEndsWhenItsRunFailedBeforePod(t *testing.T) {
	t.Parallel()
	p, kube, e, l := waitingTask(t)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if _, err := p.Prepare(ctx, l, e.initial); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued preparation: %v", err)
	}
	if r := registration(t, p, l); r.Phase != registry.Bound {
		t.Fatalf("queued registration: %+v", r)
	}
	setRun(t, kube, l, api.AgentRunPhaseFailed, "RuntimeInvalid", "AgentRuntime native is invalid: image is required")
	ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, err := p.Prepare(ctx, l, e.initial)
	requireRefused(t, err, "AgentRun "+runName(l)+" failed before its sandbox started",
		"RuntimeInvalid: AgentRuntime native is invalid: image is required")
	if r := registration(t, p, l); r.Phase != registry.Bound {
		t.Fatalf("a refused preparation changed its registration: %+v", r)
	}
}

// Each way a run can end, or stop being able to get a Pod, while Prepare
// already waits for that Pod ends the wait with the refusal.
func TestPrepareStopsWaitingWhenItsRunCanNoLongerGetAPod(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*testing.T, *Placement, *admissionClient, session.Launch)
		want   []string
	}{
		{name: "failed", change: func(t *testing.T, _ *Placement, kube *admissionClient, l session.Launch) {
			setRun(t, kube, l, api.AgentRunPhaseFailed, "PolicyNotFound", `AgentPolicy "fixture" not found`)
		}, want: []string{"failed before its sandbox started", `PolicyNotFound: AgentPolicy "fixture" not found`}},
		{name: "timed out", change: func(t *testing.T, _ *Placement, kube *admissionClient, l session.Launch) {
			setRun(t, kube, l, api.AgentRunPhaseTimedOut, "DeadlineExceeded", "the run's deadline passed")
		}, want: []string{"timed out before its sandbox started", "DeadlineExceeded"}},
		{name: "cancelled by its session", change: func(t *testing.T, _ *Placement, kube *admissionClient, l session.Launch) {
			setRun(t, kube, l, api.AgentRunPhaseCancelled, "SessionCancelled", "the session cancelled this execution")
		}, want: []string{"was cancelled before its sandbox started", "SessionCancelled"}},
		{name: "cancel requested", change: func(t *testing.T, _ *Placement, kube *admissionClient, l session.Launch) {
			run := agentRun(t, kube, l)
			run.Spec.Cancel = true
			if err := kube.Update(t.Context(), run); err != nil {
				t.Fatal(err)
			}
		}, want: []string{"was cancelled before its sandbox started"}},
		{name: "deleted", change: func(t *testing.T, _ *Placement, kube *admissionClient, l session.Launch) {
			if err := kube.Client.Delete(t.Context(), agentRun(t, kube, l)); err != nil {
				t.Fatal(err)
			}
		}, want: []string{"AgentRun " + "%run" + " was deleted before its sandbox started"}},
		{name: "registration cancelled", change: func(t *testing.T, p *Placement, _ *admissionClient, l session.Launch) {
			if _, err := p.registry.Cancel(t.Context(), registration(t, p, l).Reference); err != nil {
				t.Fatal(err)
			}
		}, want: []string{"session registration for AgentRun " + "%run" + " is cancelled"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p, kube, e, l := waitingTask(t)
			w := prepareInBackground(t, p, kube, l, e.initial)
			test.change(t, p, kube, l)
			want := make([]string, len(test.want))
			for i, s := range test.want {
				want[i] = strings.ReplaceAll(s, "%run", runName(l))
			}
			requireRefused(t, w.ended(t), want...)
		})
	}
}

// A run that is only waiting can still get its Pod: queued under a
// concurrency cap, admitted while a ResourceQuota refuses its Pod, or with
// its sandbox named and its Pod not yet created. Prepare keeps waiting
// through each, with no deadline, and prepares the visit once the Pod runs.
func TestPrepareKeepsWaitingForARunThatCanStillGetAPod(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name   string
		change func(*testing.T, *admissionClient, session.Launch)
	}{
		{name: "queued under a cap", change: func(t *testing.T, kube *admissionClient, l session.Launch) {
			setRun(t, kube, l, api.AgentRunPhaseQueued, "WaitingForConcurrencyCap", "1 of 1 active runs")
		}},
		{name: "quota exceeded", change: func(t *testing.T, kube *admissionClient, l session.Launch) {
			setRun(t, kube, l, api.AgentRunPhaseAdmitted, "QuotaExceeded", "A ResourceQuota rejected the sandbox Pod")
		}},
		{name: "sandbox named", change: func(t *testing.T, kube *admissionClient, l session.Launch) {
			run := agentRun(t, kube, l)
			run.Status.Phase = api.AgentRunPhaseRunning
			run.Status.Identity = api.AgentRunIdentity{SandboxName: "pending-sandbox", ServiceAccountName: fixtureWorker}
			if err := kube.Client.Status().Update(t.Context(), run); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p, kube, e, l := waitingTask(t)
			w := prepareInBackground(t, p, kube, l, e.initial)
			test.change(t, kube, l)
			// Three reads after the change: the wait saw it at least twice and
			// went on waiting.
			w.awaitFreshReads(t, 3)
			if _, err := createSandbox(t.Context(), kube, agentRun(t, kube, l), false); err != nil {
				t.Fatal(err)
			}
			if err := w.ended(t); err != nil {
				t.Fatalf("preparation once its Pod ran: %v", err)
			}
			if r := registration(t, p, l); r.Phase != registry.Prepared {
				t.Fatalf("registration once its Pod ran: %+v", r)
			}
		})
	}
}

// A start whose run has no sandbox yet, queued or ended, is still preparing
// to Inspect, so the coordinator's recover calls Prepare again, which waits
// or refuses, rather than failing on the missing Pod.
func TestInspectOfAStartWhoseRunHasNoSandboxIsPreparing(t *testing.T) {
	t.Parallel()
	p, kube, e, l := waitingTask(t)
	ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
	defer cancel()
	if _, err := p.Prepare(ctx, l, e.initial); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued preparation: %v", err)
	}
	for _, phase := range []api.AgentRunPhase{api.AgentRunPhaseQueued, api.AgentRunPhaseFailed} {
		setRun(t, kube, l, phase, "", "")
		o, err := p.Inspect(t.Context(), l)
		if err != nil || o.State != session.PlacementPreparing || o.Incarnation != (session.Incarnation{}) {
			t.Fatalf("Inspect of a %s run with no sandbox: %+v %v", phase, o, err)
		}
	}
}

// The refusal crosses the placement protocol with its reason, so a home
// coordinating a session on the executor ends the start as the executor's
// own coordinator would: the executor answers refused (409) with the
// reason, the home fences the preparation through abort-preparation, and
// the execution is idle with the reason in the error (ADR 0067). The home
// moves a resumed conversation, which has a deadline here: it ends at once,
// not at the deadline.
func TestHomeEndsAStartTheExecutorRefusesOverTheProtocol(t *testing.T) {
	t.Parallel()
	p, kube, _, l, data := setup(t)
	kube.queued = true
	kube.reconcile = cancelReconcile(t, kube)
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
	// The executor's answer to the home's own Prepare, while it waits.
	w := inBackground(t, kube, func() error {
		_, err := home.Prepare(t.Context(), l, data)
		return err
	})
	setRun(t, kube, l, api.AgentRunPhaseFailed, "RuntimeInvalid", "AgentRuntime native is invalid: image is required")
	const reason = "failed before its sandbox started: RuntimeInvalid: AgentRuntime native is invalid: image is required"
	err = w.ended(t)
	var refused *session.StartRefused
	var remote *profile.RemoteError
	if !errors.As(err, &remote) || remote.Code != profile.CodeRefused || remote.Status != http.StatusConflict ||
		!errors.Is(err, session.ErrStartRefused) || !errors.As(err, &refused) || !strings.Contains(refused.Reason, reason) {
		t.Fatalf("remote refused prepare: %v", err)
	}
	// The home's coordinator ends the start with that reason.
	_, err = coordinator.Recover(t.Context(), l.ExecutionID)
	if !errors.Is(err, session.ErrStartEnded) || !errors.As(err, &refused) || !strings.Contains(refused.Reason, reason) {
		t.Fatalf("remote refused start: %v", err)
	}
	r, err := coordinator.Inspect(l.ExecutionID)
	if err != nil || r.Phase != session.PhaseIdle || len(r.Visits) != 0 {
		t.Fatalf("after the remote refused start: %+v %v", r, err)
	}
	if local, err := p.load(l); err != nil || !local.PreparationAborted {
		t.Fatalf("the refused remote preparation was not fenced: %+v %v", local, err)
	}
}

// The coordinator ends a start the Kubernetes placement refuses, with no
// deadline, for each way its run can end before its Pod: the preparation is
// fenced, the execution is idle with nothing recorded as a visit, and the
// error names why (ADR 0067).
func TestCoordinatorEndsAKubernetesStartWhoseRunEndedBeforeItsPod(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		// before changes the run before the start is recovered again;
		// during, while the start waits for the Pod.
		before, during func(*testing.T, *Placement, *admissionClient, session.Launch)
		want           string
	}{
		{name: "failed before the wait", before: func(t *testing.T, _ *Placement, kube *admissionClient, l session.Launch) {
			setRun(t, kube, l, api.AgentRunPhaseFailed, "TemplateNotFound", "AgentTemplate fixture not found")
		}, want: "failed before its sandbox started: TemplateNotFound: AgentTemplate fixture not found"},
		{name: "failed while waiting", during: func(t *testing.T, _ *Placement, kube *admissionClient, l session.Launch) {
			setRun(t, kube, l, api.AgentRunPhaseFailed, "SessionBindingRejected", "the session registration is not bound")
		}, want: "failed before its sandbox started: SessionBindingRejected"},
		{name: "deleted while waiting", during: func(t *testing.T, _ *Placement, kube *admissionClient, l session.Launch) {
			if err := kube.Client.Delete(t.Context(), agentRun(t, kube, l)); err != nil {
				t.Fatal(err)
			}
		}, want: "was deleted before its sandbox started"},
		{name: "registration cancelled while waiting", during: func(t *testing.T, p *Placement, _ *admissionClient, l session.Launch) {
			if _, err := p.registry.Cancel(t.Context(), registration(t, p, l).Reference); err != nil {
				t.Fatal(err)
			}
		}, want: "is cancelled, so its sandbox can never start"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			p, kube, e, l := waitingTask(t)
			kube.reconcile = cancelReconcile(t, kube)
			l.Epoch = 1
			coordinator, err := session.Open(filepath.Join(t.TempDir(), "sessions"), map[string]session.Placement{l.Placement: p}, fixtureMax)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = coordinator.Close() })
			if _, err = coordinator.Create(l, e.initial, ""); err != nil {
				t.Fatal(err)
			}
			recoverStart := func() error {
				_, err := coordinator.Recover(t.Context(), l.ExecutionID)
				return err
			}
			if test.before != nil {
				ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
				defer cancel()
				if _, err = coordinator.Recover(ctx, l.ExecutionID); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("queued start: %v", err)
				}
				test.before(t, p, kube, l)
				err = recoverStart()
			} else {
				w := inBackground(t, kube, recoverStart)
				test.during(t, p, kube, l)
				err = w.ended(t)
			}
			var refused *session.StartRefused
			if !errors.Is(err, session.ErrStartEnded) || !errors.As(err, &refused) || !strings.Contains(refused.Reason, test.want) {
				t.Fatalf("refused Kubernetes start: %v, want it ended naming %q", err, test.want)
			}
			r, err := coordinator.Inspect(l.ExecutionID)
			if err != nil || r.Phase != session.PhaseIdle || len(r.Visits) != 0 || r.CancelOperation != "" {
				t.Fatalf("after the refused start: %+v %v", r, err)
			}
			if local, err := p.load(l); err != nil || !local.PreparationAborted {
				t.Fatalf("the refused preparation was not fenced: %+v %v", local, err)
			}
		})
	}
}
