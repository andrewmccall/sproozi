package registry

import (
	"context"
	"errors"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func managedFixture() (*api.AgentRun, *api.AgentTemplate, Registration) {
	run := &api.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: fixtureRunName, Namespace: fixtureNamespace, UID: fixtureRunUID},
		Spec: api.AgentRunSpec{TemplateRef: api.AgentTemplateReference{Name: fixtureTemplate},
			Capabilities: []api.CapabilityKind{api.CapabilityModelInference}}}
	template := &api.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Name: fixtureTemplate, Namespace: fixtureNamespace,
		Annotations: map[string]string{ManagedTemplateAnnotation: enabledValue}},
		Spec: api.AgentTemplateSpec{Instructions: "Approved mobile instructions"}}
	intent := registration()
	intent.InstructionDigest = InstructionsDigest(template.Spec.Instructions)
	intent.OriginalDeadline = time.Now().Add(time.Hour).UTC()
	return run, template, intent
}

func TestManagedBindingIsRequiredAndIncarnationBound(t *testing.T) {
	store, kube := registry(t, nil)
	ctx := context.Background()
	run, template, intent := managedFixture()
	if _, err := ResolveRun(ctx, kube, run, template); !errors.Is(err, ErrGone) {
		t.Fatalf("missing required registration: %v", err)
	}
	record, err := store.Create(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ResolveRun(ctx, kube, run, template); !errors.Is(err, ErrBindingPending) {
		t.Fatalf("unbound registration: %v", err)
	}
	if _, err = store.BindRun(ctx, record.Reference, fixtureRunUID); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveRun(ctx, kube, run, template)
	if err != nil || resolved.RunUID != fixtureRunUID {
		t.Fatalf("bound registration: %+v %v", resolved, err)
	}
	for name, mutate := range map[string]func(*api.AgentRun, *api.AgentTemplate){
		"replacement Run":      func(r *api.AgentRun, _ *api.AgentTemplate) { r.UID = "replacement" },
		"changed instructions": func(_ *api.AgentRun, t *api.AgentTemplate) { t.Spec.Instructions += " changed" },
		"widened capabilities": func(r *api.AgentRun, _ *api.AgentTemplate) {
			r.Spec.Capabilities = append(r.Spec.Capabilities, api.CapabilityKubernetesRead)
		},
	} {
		t.Run(name, func(t *testing.T) {
			r, tmpl := run.DeepCopy(), template.DeepCopy()
			mutate(r, tmpl)
			if _, err := ResolveRun(ctx, kube, r, tmpl); !errors.Is(err, ErrConflict) {
				t.Fatalf("accepted mismatched binding: %v", err)
			}
		})
	}
	delete(template.Annotations, ManagedTemplateAnnotation)
	if resolved, err = ResolveRun(ctx, kube, run, template); err != nil || resolved != nil {
		t.Fatalf("ordinary template changed by unrelated registry: %+v %v", resolved, err)
	}
}

func TestActiveBindingRequiresLiveNativeContainer(t *testing.T) {
	store, kube := registry(t, nil)
	ctx := context.Background()
	run, template, intent := managedFixture()
	run.Status.Identity = api.AgentRunIdentity{SandboxName: "sandbox", ServiceAccountName: "run-sa"}
	r, err := store.Create(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.BindRun(ctx, r.Reference, fixtureRunUID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Prepared(ctx, r.Reference, "exact-pod"); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "sandbox", Namespace: "agents", UID: types.UID("exact-pod"),
		Labels: map[string]string{"sproozi.com/agentrun-uid": fixtureRunUID}},
		Spec: corev1.PodSpec{ServiceAccountName: "run-sa"}, Status: corev1.PodStatus{Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", ContainerID: "containerd://exact-agent",
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	if err = kube.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	record, err := ResolveRun(ctx, kube, run, template)
	if err != nil {
		t.Fatal(err)
	}
	if err = RequireActive(ctx, kube, run, record, "agents"); err == nil {
		t.Fatal("prepared execution authorized gateway use")
	}
	if _, err = store.Activate(ctx, r.Reference); err != nil {
		t.Fatal(err)
	}
	record, err = ResolveRun(ctx, kube, run, template)
	if err != nil || RequireActive(ctx, kube, run, record, "agents") != nil {
		t.Fatalf("active native execution denied: %v", err)
	}
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
	if err = kube.Status().Update(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if err = RequireActive(ctx, kube, run, record, "agents"); err == nil {
		t.Fatal("running capture sidecar authorized terminated native container")
	}
}

func TestEffectiveDeadlineDoesNotResetOnAdmission(t *testing.T) {
	run, _, intent := managedFixture()
	started := metav1.NewTime(time.Now())
	run.Status.StartedAt = &started
	policy := &api.AgentPolicy{Spec: api.AgentPolicySpec{MaxExecutionDuration: metav1.Duration{Duration: time.Hour}}}
	record := &Record{Registration: intent}
	record.OriginalDeadline = started.Add(time.Minute)
	if got := EffectiveDeadline(run, policy, record); !got.Equal(record.OriginalDeadline) {
		t.Fatalf("session deadline extended: %v", got)
	}
	policy.Spec.MaxExecutionDuration.Duration = time.Second
	if got := EffectiveDeadline(run, policy, record); !got.Equal(started.Add(time.Second)) {
		t.Fatalf("live policy tightening ignored: %v", got)
	}
}
