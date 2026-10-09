package controller_test

import (
	"context"
	"errors"
	"reflect"
	"testing"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	k8sresources "github.com/andrewmccall/sproozi/internal/kubernetes"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestReconcilerRetainsAnswerWithProcessOutcomeAfterCleanup(t *testing.T) {
	for _, tc := range []struct {
		name   string
		exit   int32
		result *sprooziv1alpha1.AgentRunResult
		phase  sprooziv1alpha1.AgentRunPhase
	}{
		{"success", 0, &sprooziv1alpha1.AgentRunResult{Text: "Worker repaired café", Truncated: true}, sprooziv1alpha1.AgentRunPhaseSucceeded},
		{"failed with output", 1, &sprooziv1alpha1.AgentRunResult{Text: "I claim success"}, sprooziv1alpha1.AgentRunPhaseFailed},
		{"success unavailable", 0, nil, sprooziv1alpha1.AgentRunPhaseSucceeded},
		{"success empty", 0, &sprooziv1alpha1.AgentRunResult{Text: ""}, sprooziv1alpha1.AgentRunPhaseSucceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := &k8sresources.FakeSandboxClient{Status: k8sresources.SandboxStatusSucceeded, ExitCode: tc.exit, Result: tc.result}
			run := newRun("retained-answer")
			r, c := newReconcilerWithSandbox(sc, append(fixtures(), run)...)
			driveToRunning(t, r, c, run.Name)
			request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNS, Name: run.Name}}
			if _, err := r.Reconcile(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			got := getRun(t, c, run.Name)
			if got.Status.Phase != tc.phase || got.Status.CompletedAt == nil || !reflect.DeepEqual(got.Status.Result, tc.result) {
				t.Fatalf("completion status = %#v; want phase %s, result %#v", got.Status, tc.phase, tc.result)
			}
			if len(sc.Deleted) != 0 {
				t.Fatal("sandbox cleanup preceded terminal/result persistence")
			}
			if _, err := r.Reconcile(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			got = getRun(t, c, run.Name)
			if len(sc.Deleted) != 1 || got.Status.Identity.SandboxName != "" || !reflect.DeepEqual(got.Status.Result, tc.result) {
				t.Fatalf("retained result after cleanup = %#v, deleted %v", got.Status, sc.Deleted)
			}
		})
	}
}

type completionHookSandbox struct {
	*k8sresources.FakeSandboxClient
	hook func()
}

func (s *completionHookSandbox) GetCompletion(ctx context.Context, name, uid string) (k8sresources.SandboxCompletion, error) {
	completion, err := s.FakeSandboxClient.GetCompletion(ctx, name, uid)
	s.hook()
	return completion, err
}

func TestReconcilerCancellationDuringCompletionWinsWithoutAnswer(t *testing.T) {
	sc := &completionHookSandbox{FakeSandboxClient: &k8sresources.FakeSandboxClient{
		Status: k8sresources.SandboxStatusSucceeded, Result: &sprooziv1alpha1.AgentRunResult{Text: "late answer"},
	}}
	run := newRun("cancel-during-completion")
	r, c := newReconcilerWithSandbox(sc, append(fixtures(), run)...)
	sc.hook = func() {
		latest := getRun(t, c, run.Name)
		latest.Spec.Cancel = true
		if err := c.Update(context.Background(), &latest); err != nil {
			t.Fatal(err)
		}
	}
	driveToRunning(t, r, c, run.Name)
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNS, Name: run.Name}}
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	got := getRun(t, c, run.Name)
	if got.Status.Phase != sprooziv1alpha1.AgentRunPhaseCancelled || got.Status.Result != nil || !got.Spec.Cancel {
		t.Fatalf("cancel completion = %#v, cancel %v", got.Status, got.Spec.Cancel)
	}
}

type failTerminalStatusClient struct {
	client.Client
	fail bool
}

func (c *failTerminalStatusClient) Status() client.SubResourceWriter {
	return &failTerminalStatusWriter{SubResourceWriter: c.Client.Status(), owner: c}
}

type failTerminalStatusWriter struct {
	client.SubResourceWriter
	owner *failTerminalStatusClient
}

func (w *failTerminalStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	if run, ok := obj.(*sprooziv1alpha1.AgentRun); ok && w.owner.fail && run.Status.Phase == sprooziv1alpha1.AgentRunPhaseSucceeded {
		w.owner.fail = false
		return errors.New("interrupted terminal write")
	}
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func TestReconcilerRetriesAtomicTerminalAnswerBeforeCleanup(t *testing.T) {
	sc := &k8sresources.FakeSandboxClient{Status: k8sresources.SandboxStatusSucceeded,
		Result: &sprooziv1alpha1.AgentRunResult{Text: "durable answer"}}
	run := newRun("retry-result-write")
	r, c := newReconcilerWithSandbox(sc, append(fixtures(), run)...)
	driveToRunning(t, r, c, run.Name)
	r.Client = &failTerminalStatusClient{Client: c, fail: true}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: testNS, Name: run.Name}}
	if _, err := r.Reconcile(context.Background(), request); err == nil {
		t.Fatal("expected terminal write interruption")
	}
	got := getRun(t, c, run.Name)
	if got.Status.Phase != sprooziv1alpha1.AgentRunPhaseRunning || got.Status.Result != nil || len(sc.Deleted) != 0 {
		t.Fatalf("partially committed completion: %#v, deleted %v", got.Status, sc.Deleted)
	}
	if _, err := r.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	got = getRun(t, c, run.Name)
	if got.Status.Phase != sprooziv1alpha1.AgentRunPhaseSucceeded || got.Status.Result == nil || got.Status.Result.Text != "durable answer" || len(sc.Deleted) != 0 {
		t.Fatalf("retry completion: %#v, deleted %v", got.Status, sc.Deleted)
	}
}
