package kubernetes_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/gateway"
	gatewaykube "github.com/andrewmccall/sproozi/internal/gateway/kubernetes"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/session"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const fixtureExecutionUID = "run-uid"

type managedFixture struct {
	kube     client.Client
	finder   *gatewaykube.K8sRunFinder
	subject  gatewaykube.ServiceAccountIdentity
	store    *session.Store
	ref      session.Reference
	run      *api.AgentRun
	template *api.AgentTemplate
	policy   *api.AgentPolicy
	pod      *corev1.Pod
}

func managedExecution(t *testing.T, register bool) *managedFixture {
	t.Helper()
	cap := api.CapabilityModelInference
	f := &managedFixture{
		subject:  gatewaykube.ServiceAccountIdentity{Namespace: testAgentsNamespace, Name: testRunServiceAccount, UID: "sa-uid"},
		run:      &api.AgentRun{ObjectMeta: metav1.ObjectMeta{Name: "visit-1", Namespace: testNamespace, UID: fixtureExecutionUID}, Spec: api.AgentRunSpec{TemplateRef: api.AgentTemplateReference{Name: testTemplateName}, Capabilities: []api.CapabilityKind{cap}}, Status: api.AgentRunStatus{Phase: api.AgentRunPhaseRunning, StartedAt: &metav1.Time{Time: time.Now()}, Identity: api.AgentRunIdentity{ServiceAccountName: testRunServiceAccount, SandboxName: "worker"}}},
		template: &api.AgentTemplate{ObjectMeta: metav1.ObjectMeta{Name: testTemplateName, Namespace: testNamespace, UID: "template-uid", Annotations: map[string]string{session.ManagedTemplateAnnotation: "true"}}, Spec: api.AgentTemplateSpec{PolicyRef: api.AgentPolicyReference{Name: testPolicyName}, Instructions: "trusted instructions"}},
		policy:   &api.AgentPolicy{ObjectMeta: metav1.ObjectMeta{Name: testPolicyName, Namespace: testNamespace, UID: "policy-uid", Generation: 1}, Spec: api.AgentPolicySpec{MaxExecutionDuration: metav1.Duration{Duration: time.Hour}, AllowedCapabilities: []api.CapabilityKind{cap}, Budgets: map[api.CapabilityKind]api.EndpointBudget{cap: {MaxUnits: 100}}}},
		pod:      &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: testAgentsNamespace, UID: "pod-uid", Labels: map[string]string{testRunUIDLabel: fixtureExecutionUID}}, Spec: corev1.PodSpec{ServiceAccountName: testRunServiceAccount}, Status: corev1.PodStatus{Phase: corev1.PodRunning, ContainerStatuses: []corev1.ContainerStatus{{Name: "agent", ContainerID: "container://native", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}},
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: f.subject.Name, Namespace: f.subject.Namespace, UID: types.UID(f.subject.UID), Labels: map[string]string{testManagedByLabel: testManagedByValue, testRunLabel: f.run.Name, testRunNamespaceLabel: f.run.Namespace, testRunUIDLabel: string(f.run.UID)}}}
	f.kube = fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&api.AgentRun{}).WithObjects(f.run, f.template, f.policy, f.pod, sa).WithInterceptorFuncs(interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
		if obj.GetUID() == "" {
			obj.SetUID("registry-uid")
		}
		return c.Create(ctx, obj, opts...)
	}}).Build()
	f.finder = gatewaykube.NewK8sRunFinder(f.kube, testAgentsNamespace)
	var err error
	f.store, err = session.NewStore(f.kube, testNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if register {
		record, err := f.store.Create(t.Context(), session.Registration{Harness: harness.NativeCodex, Principal: "operator", SessionID: "session-1", Epoch: 1, OperationID: "move-1", NativeUUID: "12345678-1234-1234-1234-123456789abc", NativePin: "0.161.0", InstructionDigest: session.InstructionsDigest(f.template.Spec.Instructions), OriginalDeadline: time.Now().Add(time.Hour), TemplateName: f.template.Name, Ceilings: map[api.CapabilityKind]api.EndpointBudget{cap: {MaxUnits: 2}}, CheckpointDigest: strings.Repeat("b", 64), IntendedRunName: f.run.Name})
		if err != nil {
			t.Fatal(err)
		}
		f.ref = record.Reference
		if _, err = f.store.BindRun(t.Context(), f.ref, string(f.run.UID)); err != nil {
			t.Fatal(err)
		}
		if _, err = f.store.Prepared(t.Context(), f.ref, string(f.pod.UID)); err != nil {
			t.Fatal(err)
		}
		if _, err = f.store.Activate(t.Context(), f.ref); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func TestManagedRunFinderResolvesConcreteExecutionAllocation(t *testing.T) {
	f := managedExecution(t, true)
	grant, err := f.finder.Find(t.Context(), f.subject)
	if err != nil {
		t.Fatal(err)
	}
	a := grant.Allocations[api.CapabilityModelInference]
	if !grant.Managed || grant.Execution.Backend != gateway.BackendKubernetes || grant.Execution.UID != fixtureExecutionUID || a.AccountKey != "run-uid/model.inference" || a.Ceiling.MaxUnits != 2 {
		t.Fatalf("resolved grant: %+v allocation: %+v", grant, a)
	}
	if err := f.finder.Revalidate(t.Context(), grant); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Stop(t.Context(), f.ref); err != nil {
		t.Fatal(err)
	}
	if err := f.finder.Revalidate(t.Context(), grant); !errors.Is(err, gateway.ErrGrantRevoked) {
		t.Fatalf("stopping native worker retained authority: %v", err)
	}
}

func TestManagedRunFinderDeniesMissingRegistration(t *testing.T) {
	f := managedExecution(t, false)
	if grant, err := f.finder.Find(t.Context(), f.subject); err == nil || grant != nil {
		t.Fatalf("unregistered managed template authenticated: %+v %v", grant, err)
	}
}

func TestManagedRunFinderRevalidatesNativeWorkerAndTrustedInstructions(t *testing.T) {
	for _, name := range []string{"changed pod UID", "terminated native container", "changed instructions", "changed policy", "ordinary expired deadline"} {
		t.Run(name, func(t *testing.T) {
			f := managedExecution(t, true)
			grant, err := f.finder.Find(t.Context(), f.subject)
			if err != nil {
				t.Fatal(err)
			}
			switch name {
			case "changed pod UID":
				f.pod.UID = "replacement-pod"
				err = f.kube.Update(t.Context(), f.pod)
			case "terminated native container":
				f.pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
				err = f.kube.Status().Update(t.Context(), f.pod)
			case "changed instructions":
				f.template.Spec.Instructions = "revised instructions"
				err = f.kube.Update(t.Context(), f.template)
			case "changed policy":
				f.policy.Generation++
				f.policy.Spec.Budgets[api.CapabilityModelInference] = api.EndpointBudget{MaxUnits: 1}
				err = f.kube.Update(t.Context(), f.policy)
			case "ordinary expired deadline":
				delete(f.template.Annotations, session.ManagedTemplateAnnotation)
				err = f.kube.Update(t.Context(), f.template)
				if err == nil {
					f.run.Status.StartedAt = &metav1.Time{Time: time.Now().Add(-2 * time.Hour)}
					err = f.kube.Status().Update(t.Context(), f.run)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := f.finder.Revalidate(t.Context(), grant); !errors.Is(err, gateway.ErrGrantRevoked) {
				t.Fatalf("changed native ownership retained grant: %v", err)
			}
		})
	}
}
