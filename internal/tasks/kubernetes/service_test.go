package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/tasks"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	fixtureNamespace = "ops"
	fixtureWorkflow  = "investigate"
	fixturePolicy    = "bounded"
	fixtureUID       = "worker-1"
)

func fixtureService(t *testing.T) (*Service, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := api.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&api.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Name: fixtureWorkflow, Namespace: fixtureNamespace}, Spec: api.AgentTemplateSpec{PolicyRef: api.AgentPolicyReference{Name: fixturePolicy}}},
		&api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: fixturePolicy, Namespace: fixtureNamespace}, Spec: api.AgentPolicySpec{RetentionTTL: metav1.Duration{Duration: 24 * time.Hour}}},
	).Build()
	service, err := NewService(kube, Config{Namespace: fixtureNamespace, Principal: "hermes", Workflows: map[string]Workflow{fixtureWorkflow: {Template: fixtureWorkflow, Capabilities: []api.CapabilityKind{api.CapabilityModelInference}}}})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return time.Date(2026, 10, 8, 20, 0, 0, 0, time.UTC) }
	return service, kube
}

func fixtureRequest() tasks.Submit {
	return tasks.Submit{Workflow: fixtureWorkflow, RequestID: "incident-1", ExpiresAt: "2026-10-08T20:30:00Z", Task: "Explain the supplied failure"}
}

func TestSubmitReplayConflictAndExpiry(t *testing.T) {
	service, kube := fixtureService(t)
	ctx := context.Background()
	input := fixtureRequest()
	first, err := service.Submit(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	var run api.AgentRun
	if err = kube.Get(ctx, client.ObjectKey{Namespace: fixtureNamespace, Name: first.ID}, &run); err != nil {
		t.Fatal(err)
	}
	if run.Spec.TemplateRef.Name != fixtureWorkflow || run.Spec.Task != "Explain the supplied failure" || len(run.Spec.Capabilities) != 1 || run.Spec.Capabilities[0] != "model.inference" {
		t.Fatalf("unexpected worker authority: %#v", run.Spec)
	}
	// Real API assigns UID; a deterministic fake UID makes incarnation assertions explicit.
	run.UID = types.UID(fixtureUID)
	if err = kube.Update(ctx, &run); err != nil {
		t.Fatal(err)
	}
	replay, err := service.Submit(ctx, input)
	if err != nil || replay.UID != fixtureUID || replay.ID != first.ID {
		t.Fatalf("replay: %#v %v", replay, err)
	}
	input.Task = "Perform a different task"
	if _, err = service.Submit(ctx, input); !errors.Is(err, tasks.ErrConflict) {
		t.Fatalf("changed task accepted: %v", err)
	}
	input = fixtureRequest()
	input.ExpiresAt = "2026-10-08T20:45:00Z"
	if _, err = service.Submit(ctx, input); !errors.Is(err, tasks.ErrConflict) {
		t.Fatalf("extended replay deadline accepted: %v", err)
	}
	service.now = func() time.Time { return time.Date(2026, 10, 8, 22, 0, 0, 0, time.UTC) }
	if replay, err = service.Submit(ctx, fixtureRequest()); err != nil || replay.UID != fixtureUID {
		t.Fatalf("retained expired replay: %#v %v", replay, err)
	}
	if err = kube.Delete(ctx, &run); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Submit(ctx, fixtureRequest()); !errors.Is(err, tasks.ErrExpired) {
		t.Fatalf("expired missing request recreated: %v", err)
	}
}

func TestStatusKeepsLiteralNeutralLifecycleAndUntrustedResult(t *testing.T) {
	service, kube := fixtureService(t)
	first, err := service.Submit(t.Context(), fixtureRequest())
	if err != nil {
		t.Fatal(err)
	}
	var run api.AgentRun
	if err = kube.Get(t.Context(), client.ObjectKey{Namespace: fixtureNamespace, Name: first.ID}, &run); err != nil {
		t.Fatal(err)
	}
	run.UID = fixtureUID
	run.Status.Phase = api.AgentRunPhaseFailed
	run.Status.StartedAt = &metav1.Time{Time: time.Date(2026, 10, 8, 20, 1, 0, 0, time.UTC)}
	run.Status.CompletedAt = &metav1.Time{Time: time.Date(2026, 10, 8, 20, 2, 0, 0, time.UTC)}
	run.Status.Result = &api.AgentRunResult{Text: "", Truncated: true}
	if err = kube.Update(t.Context(), &run); err != nil {
		t.Fatal(err)
	}
	view, err := service.Status(t.Context(), tasks.Reference{ID: first.ID, UID: fixtureUID})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(view)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"id":"task-7131d2b16aa0d3cbf705b2170ca8720542728094","uid":"worker-1","phase":"Failed","cancellationRequested":false,"startedAt":"2026-10-08T20:01:00Z","completedAt":"2026-10-08T20:02:00Z","result":{"text":"","truncated":true}}`
	if string(data) != want {
		t.Fatalf("neutral task observation changed: %s", data)
	}
}

func TestCallerAndIncarnationBoundCancellation(t *testing.T) {
	service, kube := fixtureService(t)
	ctx := context.Background()
	first, err := service.Submit(ctx, fixtureRequest())
	if err != nil {
		t.Fatal(err)
	}
	var run api.AgentRun
	if err = kube.Get(ctx, client.ObjectKey{Namespace: fixtureNamespace, Name: first.ID}, &run); err != nil {
		t.Fatal(err)
	}
	run.UID = fixtureUID
	if err = kube.Update(ctx, &run); err != nil {
		t.Fatal(err)
	}
	ref := tasks.Reference{ID: first.ID, UID: fixtureUID}
	if _, err = service.Cancel(ctx, tasks.Reference{ID: first.ID, UID: "replacement"}); !errors.Is(err, tasks.ErrGone) {
		t.Fatalf("stale UID: %v", err)
	}
	other := *service
	other.config.Principal = "other-assistant"
	if _, err = other.Status(ctx, ref); !errors.Is(err, tasks.ErrDenied) {
		t.Fatalf("other caller read: %v", err)
	}
	if _, err = other.Cancel(ctx, ref); !errors.Is(err, tasks.ErrDenied) {
		t.Fatalf("other caller cancelled: %v", err)
	}
	observation, err := service.Cancel(ctx, ref)
	if err != nil || !observation.CancellationRequested {
		t.Fatalf("cancel: %#v %v", observation, err)
	}
	observation, err = service.Cancel(ctx, ref)
	if err != nil || !observation.CancellationRequested {
		t.Fatalf("repeat cancel: %#v %v", observation, err)
	}
	if _, err = service.Submit(ctx, fixtureRequest()); err != nil {
		t.Fatalf("cancellation broke replay: %v", err)
	}
}

func TestSubmitChecksLiveRetentionAndFixedWorkflow(t *testing.T) {
	service, kube := fixtureService(t)
	ctx := context.Background()
	request := fixtureRequest()
	request.Workflow = "unapproved"
	if _, err := service.Submit(ctx, request); !errors.Is(err, tasks.ErrDenied) {
		t.Fatalf("workflow widened: %v", err)
	}
	var policy api.AgentPolicy
	if err := kube.Get(ctx, client.ObjectKey{Namespace: fixtureNamespace, Name: fixturePolicy}, &policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.RetentionTTL.Duration = time.Minute
	if err := kube.Update(ctx, &policy); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Submit(ctx, fixtureRequest()); !errors.Is(err, tasks.ErrDenied) {
		t.Fatalf("shortened retention: %v", err)
	}
	var runs api.AgentRunList
	if err := kube.List(ctx, &runs); err != nil {
		t.Fatal(err)
	}
	if len(runs.Items) != 0 {
		t.Fatal("denied submission created work")
	}
}

func TestInvalidSubmissionDoesNotCreateWork(t *testing.T) {
	service, kube := fixtureService(t)
	ctx := context.Background()
	for _, change := range []func(*tasks.Submit){
		func(r *tasks.Submit) { r.ExpiresAt = "2026-10-08T23:00:00Z" },
		func(r *tasks.Submit) { r.ExpiresAt = "forever" },
		func(r *tasks.Submit) { r.RequestID = "../escape" },
		func(r *tasks.Submit) { r.Task = "" },
	} {
		request := fixtureRequest()
		change(&request)
		if _, err := service.Submit(ctx, request); !errors.Is(err, tasks.ErrInvalid) {
			t.Fatalf("invalid request accepted: %v", err)
		}
	}
	var runs api.AgentRunList
	if err := kube.List(ctx, &runs); err != nil {
		t.Fatal(err)
	}
	if len(runs.Items) != 0 {
		t.Fatal("invalid request created work")
	}
}

// stalledReader blocks every read until its context ends. A read without a
// deadline is refused at once and recorded, so an unbounded wait fails the
// test instead of hanging it.
type stalledReader struct {
	client.Client
	unbounded *atomic.Bool
}

func (s stalledReader) Get(ctx context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	if _, bounded := ctx.Deadline(); !bounded {
		s.unbounded.Store(true)
		return errors.New("unbounded API read")
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestWaitBoundsAStalledAPIRead(t *testing.T) {
	service, kube := fixtureService(t)
	reader := stalledReader{Client: kube, unbounded: &atomic.Bool{}}
	service.kube = reader
	// The caller sets no deadline, so any bound on the read comes from Wait.
	_, err := service.Wait(t.Context(), tasks.Reference{ID: "task-stalled", UID: fixtureUID}, 25*time.Millisecond)
	if !errors.Is(err, tasks.ErrUnavailable) || reader.unbounded.Load() {
		t.Fatalf("wait did not bound a stalled API read: %v", err)
	}
}
