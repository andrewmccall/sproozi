package controller_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/controller"
	"github.com/andrewmccall/sproozi/internal/harness"
	k8sresources "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
)

// managedSandbox is the fake sandbox client with the Pod-backed client's
// managed path: EnsureManaged builds the real managed Pod, so the real
// managed-shape rules refuse a wrong runtime exactly where they do in the
// cluster. Built Pods are recorded, never created.
type managedSandbox struct {
	*k8sresources.FakeSandboxClient
	managed []string
}

func (s *managedSandbox) EnsureManaged(_ context.Context, run *sprooziv1alpha1.AgentRun, rt *sprooziv1alpha1.AgentRuntime,
	binding *registry.Record,
) (string, error) {
	pod, err := k8sresources.BuildManagedPodSpec(run, rt, binding)
	if err != nil {
		return "", err
	}
	s.managed = append(s.managed, pod.Name)
	return pod.Name, nil
}

// withManagedShape turns the fixture runtime into a managed Codex runtime:
// the agent plus a checkpoint helper.
func withManagedShape(rt *sprooziv1alpha1.AgentRuntime) {
	rt.Spec.ClientConfig.Harness = string(harness.NativeCodex)
	helper := *rt.Spec.PodTemplate.Spec.Containers[0].DeepCopy()
	helper.Name = k8sresources.CaptureContainer
	rt.Spec.PodTemplate.Spec.Containers = append(rt.Spec.PodTemplate.Spec.Containers, helper)
}

const (
	managedRuntimeName  = "native"
	managedTemplateName = "native-session"
	managedRunName      = "managed"
	ordinaryRunName     = "ordinary"
)

// managedShapeCluster stores a managed runtime (the fixture runtime with the
// Codex harness and a checkpoint helper, edited by shape), a managed template
// using it, a bound registration and an older queued managed run, plus a newer
// queued ordinary run under the same policy.
func managedShapeCluster(t *testing.T, shape func(*sprooziv1alpha1.AgentRuntime)) (*controller.AgentRunReconciler, client.Client, *managedSandbox) {
	t.Helper()
	objs := fixtures()
	rt := objs[0].(*sprooziv1alpha1.AgentRuntime).DeepCopy()
	rt.Name = managedRuntimeName
	withManagedShape(rt)
	if shape != nil {
		shape(rt)
	}
	tmpl := objs[2].(*sprooziv1alpha1.AgentTemplate).DeepCopy()
	tmpl.Name = managedTemplateName
	tmpl.Spec.RuntimeRef.Name = managedRuntimeName
	tmpl.Annotations = map[string]string{registry.ManagedTemplateAnnotation: managedValue}

	managedRun := newRun(managedRunName)
	managedRun.Spec.TemplateRef.Name = managedTemplateName
	managedRun.Status.Phase = sprooziv1alpha1.AgentRunPhaseQueued
	managedRun.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Minute))
	ordinary := newRun(ordinaryRunName)
	ordinary.Status.Phase = sprooziv1alpha1.AgentRunPhaseQueued
	ordinary.CreationTimestamp = metav1.NewTime(time.Now())

	c := fake.NewClientBuilder().
		WithScheme(newScheme()).
		WithObjects(append(objs, rt, tmpl, managedRun, ordinary)...).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if obj.GetUID() == "" {
					obj.SetUID(types.UID("uid-" + obj.GetName()))
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	store, err := registry.NewStore(c, testNS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(context.Background(), registry.Registration{
		Harness: harness.NativeCodex, Principal: testPrincipal, SessionID: testSessionID, Epoch: 1, OperationID: testOperationID,
		InstructionDigest: registry.InstructionsDigest(tmpl.Spec.Instructions),
		OriginalDeadline:  time.Now().Add(time.Hour), TemplateName: tmpl.Name,
		Ceilings: map[sprooziv1alpha1.CapabilityKind]sprooziv1alpha1.EndpointBudget{
			sprooziv1alpha1.CapabilityKubernetesRead: {MaxUnits: 10}},
		NativeUUID: testNativeUUID, CheckpointDigest: strings.Repeat("b", 64), IntendedRunName: managedRunName,
	}); err != nil {
		t.Fatalf("Create registration: %v", err)
	}
	editRegistration(t, c, registry.NameForRun(managedRunName), func(doc map[string]any) {
		doc["phase"] = string(registry.Bound)
		doc["runUid"] = string(managedRun.UID)
	})

	sandbox := &managedSandbox{FakeSandboxClient: &k8sresources.FakeSandboxClient{Status: k8sresources.SandboxStatusRunning}}
	r := &controller.AgentRunReconciler{Client: c, APIReader: c, SandboxClient: sandbox, MaxActiveRuns: 1}
	return r, c, sandbox
}

func reconcileNamed(r *controller.AgentRunReconciler, name string) (ctrl.Result, error) {
	return r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: name, Namespace: testNS}})
}

// runObjects counts what admission provisions for a run: its ServiceAccount,
// NetworkPolicy and agent contract ConfigMap.
func runObjects(t *testing.T, c client.Client) (accounts, policies, contracts int) {
	t.Helper()
	var sas corev1.ServiceAccountList
	var nps networkingv1.NetworkPolicyList
	var cms corev1.ConfigMapList
	for _, list := range []client.ObjectList{&sas, &nps, &cms} {
		if err := c.List(context.Background(), list); err != nil {
			t.Fatal(err)
		}
	}
	for _, cm := range cms.Items {
		if !strings.HasPrefix(cm.Name, "sproozi-session-") && cm.Name != registry.NameForRun(managedRunName) {
			contracts++
		}
	}
	return len(sas.Items), len(nps.Items), contracts
}

// wrongManagedShapes breaks one managed-shape rule each, keeping the ordinary
// template check satisfied; the full table of rules is in the kubernetes
// package's TestManagedShapeRulesHaveOneSource.
var wrongManagedShapes = map[string]struct {
	shape func(*sprooziv1alpha1.AgentRuntime)
	rule  string
}{
	"no checkpoint helper": {func(rt *sprooziv1alpha1.AgentRuntime) {
		rt.Spec.PodTemplate.Spec.Containers = rt.Spec.PodTemplate.Spec.Containers[:1]
	}, "managed runtime requires one native agent and one checkpoint helper"},
	"helper renamed": {func(rt *sprooziv1alpha1.AgentRuntime) {
		rt.Spec.PodTemplate.Spec.Containers[1].Name = "sidecar"
	}, "managed runtime checkpoint helper is missing"},
	"helper environment": {func(rt *sprooziv1alpha1.AgentRuntime) {
		rt.Spec.PodTemplate.Spec.Containers[1].Env = []corev1.EnvVar{{Name: "LEAK", Value: "x"}}
	}, "checkpoint helper cannot receive credentials, mounts or workload hooks"},
	"reserved volume": {func(rt *sprooziv1alpha1.AgentRuntime) {
		rt.Spec.PodTemplate.Spec.Volumes = append(rt.Spec.PodTemplate.Spec.Volumes, corev1.Volume{Name: "checkpoint-tmp",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: ptr.To(resource.MustParse("1Mi"))}}})
	}, "checkpoint temporary volume is reserved"},
}

// A managed run whose runtime has the wrong managed shape must end at
// admission like any other invalid runtime. Before, it was admitted, showed
// Ready Running, held the concurrency slot and failed in EnsureManaged on
// every reconcile until its deadline, or without end when it had none.
func TestReconcilerManagedRunWithWrongShapeFailsAtAdmission(t *testing.T) {
	t.Parallel()
	for name, tc := range wrongManagedShapes {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, c, sandbox := managedShapeCluster(t, tc.shape)
			for range 3 {
				if _, err := reconcileNamed(r, managedRunName); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
			}
			run := getRun(t, c, managedRunName)
			if run.Status.Phase != sprooziv1alpha1.AgentRunPhaseFailed || run.Status.StartedAt != nil {
				t.Fatalf("phase = %q, startedAt = %v; want Failed without admission", run.Status.Phase, run.Status.StartedAt)
			}
			assertReadyMessage(t, c, managedRunName, "RuntimeInvalid", "AgentRuntime "+managedRuntimeName, tc.rule)
			if sa, np, cm := runObjects(t, c); sa+np+cm != 0 || len(sandbox.managed) != 0 {
				t.Fatalf("left behind %d ServiceAccounts, %d NetworkPolicies, %d contracts, Pods %v", sa, np, cm, sandbox.managed)
			}
			if _, err := reconcileNamed(r, ordinaryRunName); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if phase := getPhase(t, c, ordinaryRunName); phase != sprooziv1alpha1.AgentRunPhaseAdmitted {
				t.Fatalf("next run phase = %q, want Admitted: the failed run held the cap", phase)
			}
		})
	}
}

// A managed run whose runtime names another harness than its registration,
// or none, must end at admission naming both. Before, it was admitted, showed
// Ready Running, held the slot and failed building its agent contract on
// every reconcile until its deadline, or without end when it had none.
func TestReconcilerManagedRunWithWrongHarnessFailsAtAdmission(t *testing.T) {
	t.Parallel()
	for name, runtimeHarness := range map[string]string{"another harness": string(harness.NativeClaude), "no harness": ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, c, sandbox := managedShapeCluster(t, func(rt *sprooziv1alpha1.AgentRuntime) {
				rt.Spec.ClientConfig.Harness = runtimeHarness
			})
			for range 3 {
				if _, err := reconcileNamed(r, managedRunName); err != nil {
					t.Fatalf("Reconcile() error = %v", err)
				}
			}
			run := getRun(t, c, managedRunName)
			if run.Status.Phase != sprooziv1alpha1.AgentRunPhaseFailed || run.Status.StartedAt != nil {
				t.Fatalf("phase = %q, startedAt = %v; want Failed without admission", run.Status.Phase, run.Status.StartedAt)
			}
			assertReadyMessage(t, c, managedRunName, "RuntimeInvalid", "AgentRuntime "+managedRuntimeName,
				`harness "`+runtimeHarness+`"`, `harness "`+string(harness.NativeCodex)+`"`)
			if sa, np, cm := runObjects(t, c); sa+np+cm != 0 || len(sandbox.managed) != 0 {
				t.Fatalf("left behind %d ServiceAccounts, %d NetworkPolicies, %d contracts, Pods %v", sa, np, cm, sandbox.managed)
			}
			if _, err := reconcileNamed(r, ordinaryRunName); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if phase := getPhase(t, c, ordinaryRunName); phase != sprooziv1alpha1.AgentRunPhaseAdmitted {
				t.Fatalf("next run phase = %q, want Admitted: the failed run held the cap", phase)
			}
		})
	}
}

// The fixture is a correctly shaped managed runtime: its run is admitted and
// gets its managed Pod. This keeps the test above honest.
func TestReconcilerManagedRunWithRightShapeStarts(t *testing.T) {
	t.Parallel()
	r, c, sandbox := managedShapeCluster(t, nil)
	for range 3 {
		if _, err := reconcileNamed(r, managedRunName); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}
	if phase := getPhase(t, c, managedRunName); phase != sprooziv1alpha1.AgentRunPhaseRunning || len(sandbox.managed) != 1 {
		t.Fatalf("phase = %q, managed Pods %v; want Running with one", phase, sandbox.managed)
	}
}

// The managed-shape rules apply only to managed runs: an ordinary run on a
// runtime that breaks them is admitted as before.
func TestReconcilerOrdinaryRunIgnoresManagedShape(t *testing.T) {
	t.Parallel()
	r, c, _ := managedShapeCluster(t, wrongManagedShapes["helper environment"].shape)
	var tmpl sprooziv1alpha1.AgentTemplate
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: newRun(ordinaryRunName).Spec.TemplateRef.Name}, &tmpl); err != nil {
		t.Fatal(err)
	}
	tmpl.Spec.RuntimeRef.Name = managedRuntimeName
	if err := c.Update(context.Background(), &tmpl); err != nil {
		t.Fatal(err)
	}
	r.MaxActiveRuns = 0
	for range 3 {
		if _, err := reconcileNamed(r, ordinaryRunName); err != nil {
			t.Fatalf("Reconcile() error = %v", err)
		}
	}
	if phase := getPhase(t, c, ordinaryRunName); phase != sprooziv1alpha1.AgentRunPhaseRunning {
		t.Fatalf("ordinary run phase = %q, want Running", phase)
	}
}

// runtimeEdits break one runtime rule each, on a runtime that had them all
// when the run was admitted.
var runtimeEdits = map[string]struct {
	edit func(*sprooziv1alpha1.AgentRuntime)
	rule string
}{
	"template rule": {func(rt *sprooziv1alpha1.AgentRuntime) {
		rt.Spec.PodTemplate.Spec.Containers[0].Image = "ghcr.io/openai/codex:latest"
	}, `container "agent" image must be digest pinned`},
	"managed shape": {wrongManagedShapes["helper environment"].shape, wrongManagedShapes["helper environment"].rule},
	"harness": {func(rt *sprooziv1alpha1.AgentRuntime) { rt.Spec.ClientConfig.Harness = string(harness.NativeClaude) },
		`managed runtime harness "claude-code" does not match the session's harness "codex"`},
}

// editRuntime applies edit to the stored runtime named name.
func editRuntime(t *testing.T, c client.Client, name string, edit func(*sprooziv1alpha1.AgentRuntime)) {
	t.Helper()
	var rt sprooziv1alpha1.AgentRuntime
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, &rt); err != nil {
		t.Fatal(err)
	}
	edit(&rt)
	if err := c.Update(context.Background(), &rt); err != nil {
		t.Fatal(err)
	}
}

// reconcileTimes reconciles the named run n times, failing on any error.
func reconcileTimes(t *testing.T, r *controller.AgentRunReconciler, name string, n int) {
	t.Helper()
	for range n {
		if _, err := reconcileNamed(r, name); err != nil {
			t.Fatalf("Reconcile(%s) error = %v", name, err)
		}
	}
}

// A run that has its Pod keeps running when its runtime is edited to break a
// rule, and when the edit is later reverted, and still ends when its Pod
// does: runtime rules gate the building of a Pod, not a running one (ADR
// 0077). Before, the edit failed the run with RuntimeInvalid and released its
// sandbox, losing a session's work since its last checkpoint.
func TestReconcilerRunWithPodKeepsRunningWhenRuntimeEdited(t *testing.T) {
	t.Parallel()
	type tc struct {
		run, runtime string
		edit         func(*sprooziv1alpha1.AgentRuntime)
	}
	cases := map[string]tc{
		"ordinary run, template rule": {ordinaryRunName, fixtures()[0].GetName(), runtimeEdits["template rule"].edit},
	}
	for name, edit := range runtimeEdits {
		cases["managed session, "+name] = tc{managedRunName, managedRuntimeName, edit.edit}
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r, c, sandbox := managedShapeCluster(t, nil)
			r.MaxActiveRuns = 0
			reconcileTimes(t, r, tc.run, 3) // Queued → Admitted → Running → Pod
			pods := func() int { return len(sandbox.Ensured) + len(sandbox.managed) }
			if run := getRun(t, c, tc.run); run.Status.Phase != sprooziv1alpha1.AgentRunPhaseRunning ||
				run.Status.Identity.SandboxName == "" || pods() != 1 {
				t.Fatalf("setup: phase %q sandbox %q pods %d", run.Status.Phase, run.Status.Identity.SandboxName, pods())
			}
			var original sprooziv1alpha1.AgentRuntime
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: tc.runtime}, &original); err != nil {
				t.Fatal(err)
			}
			stillRunning := func(when string) {
				t.Helper()
				run := getRun(t, c, tc.run)
				ready := apimeta.FindStatusCondition(run.Status.Conditions, "Ready")
				if run.Status.Phase != sprooziv1alpha1.AgentRunPhaseRunning || ready == nil || ready.Status != metav1.ConditionTrue ||
					len(sandbox.Deleted) != 0 || pods() != 1 {
					t.Fatalf("%s: phase %q ready %+v deleted %v pods %d; want the run running on its one Pod",
						when, run.Status.Phase, ready, sandbox.Deleted, pods())
				}
			}
			editRuntime(t, c, tc.runtime, tc.edit)
			reconcileTimes(t, r, tc.run, 3)
			stillRunning("after the edit")
			editRuntime(t, c, tc.runtime, func(rt *sprooziv1alpha1.AgentRuntime) { rt.Spec = original.Spec })
			reconcileTimes(t, r, tc.run, 2)
			stillRunning("after the fix")
			editRuntime(t, c, tc.runtime, tc.edit)
			sandbox.Status = k8sresources.SandboxStatusSucceeded
			reconcileTimes(t, r, tc.run, 1)
			if phase := getPhase(t, c, tc.run); phase != sprooziv1alpha1.AgentRunPhaseSucceeded {
				t.Fatalf("phase after its Pod finished on the edited runtime = %q, want Succeeded", phase)
			}
		})
	}
}

// A run admitted on a valid runtime that is edited before its Pod is built
// fails with RuntimeInvalid when the Pod would be built, takes no Pod and
// releases its place, whatever rule the edit breaks.
func TestReconcilerRuntimeEditedBeforeThePodFailsTheRun(t *testing.T) {
	t.Parallel()
	for name, edit := range runtimeEdits {
		for phase, reconciles := range map[string]int{"Admitted": 1, "Running": 2} {
			t.Run(name+" while "+phase, func(t *testing.T) {
				t.Parallel()
				r, c, sandbox := managedShapeCluster(t, nil)
				reconcileTimes(t, r, managedRunName, reconciles)
				if run := getRun(t, c, managedRunName); string(run.Status.Phase) != phase || run.Status.Identity.SandboxName != "" {
					t.Fatalf("setup: phase %q sandbox %q", run.Status.Phase, run.Status.Identity.SandboxName)
				}
				editRuntime(t, c, managedRuntimeName, edit.edit)
				reconcileTimes(t, r, managedRunName, 3)
				if phase := getPhase(t, c, managedRunName); phase != sprooziv1alpha1.AgentRunPhaseFailed {
					t.Fatalf("phase = %q, want Failed", phase)
				}
				assertReadyMessage(t, c, managedRunName, "RuntimeInvalid", "AgentRuntime "+managedRuntimeName, edit.rule)
				if len(sandbox.managed) != 0 {
					t.Fatalf("managed Pods %v built on the broken runtime", sandbox.managed)
				}
				reconcileTimes(t, r, ordinaryRunName, 1)
				if phase := getPhase(t, c, ordinaryRunName); phase != sprooziv1alpha1.AgentRunPhaseAdmitted {
					t.Fatalf("next run phase = %q, want Admitted: the failed run held the cap", phase)
				}
			})
		}
	}
}

// An ErrInvalidRuntime from building the Pod itself, as when the runtime is
// edited between the check and the build, ends the run with RuntimeInvalid
// instead of retrying for ever.
func TestReconcilerInvalidRuntimeFromThePodBuildIsTerminal(t *testing.T) {
	t.Parallel()
	run := newRun(runOne)
	run.Status.Phase = sprooziv1alpha1.AgentRunPhaseQueued
	sandbox := &k8sresources.FakeSandboxClient{EnsureErr: fmt.Errorf("%w: gatewayEndpoint has no host", k8sresources.ErrInvalidRuntime)}
	r, c := newReconcilerWithSandbox(sandbox, append(fixtures(), run)...)
	reconcileTimes(t, r, runOne, 3)
	if phase := getPhase(t, c, runOne); phase != sprooziv1alpha1.AgentRunPhaseFailed {
		t.Fatalf("phase = %q, want Failed", phase)
	}
	assertReadyMessage(t, c, runOne, "RuntimeInvalid", "AgentRuntime codex: cannot launch a sandbox: gatewayEndpoint has no host")
}

// A policy tightening still fails an active run that has its Pod (ADR 0027;
// ADR 0077 changes only what a runtime edit does).
func TestReconcilerPolicyTighteningFailsARunningRun(t *testing.T) {
	t.Parallel()
	r, c, sandbox := managedShapeCluster(t, nil)
	r.MaxActiveRuns = 0
	reconcileTimes(t, r, ordinaryRunName, 3)
	if run := getRun(t, c, ordinaryRunName); run.Status.Identity.SandboxName == "" {
		t.Fatal("setup: no Pod")
	}
	var pol sprooziv1alpha1.AgentPolicy
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: testPolicy}, &pol); err != nil {
		t.Fatal(err)
	}
	pol.Spec.AllowedCapabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityGitHubPullRequest}
	if err := c.Update(context.Background(), &pol); err != nil {
		t.Fatal(err)
	}
	reconcileTimes(t, r, ordinaryRunName, 2)
	if phase := getPhase(t, c, ordinaryRunName); phase != sprooziv1alpha1.AgentRunPhaseFailed {
		t.Fatalf("phase = %q, want Failed", phase)
	}
	assertReadyMessage(t, c, ordinaryRunName, "CapabilityNotAllowed")
	if len(sandbox.Deleted) == 0 {
		t.Fatal("the denied run's sandbox was not released")
	}
}
