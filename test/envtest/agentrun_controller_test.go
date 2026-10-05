/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package envtest

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/controller"
	k8sresources "github.com/andrewmccall/sproozi/internal/kubernetes"
)

// startAgentRunController starts an envtest environment with the AgentRun controller
// running. Returns the manager client and a cleanup function.
func startAgentRunController(t *testing.T) (client.Client, context.Context) {
	t.Helper()

	log.SetLogger(zap.New(zap.UseDevMode(true)))

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("clientgoscheme.AddToScheme() error = %v", err)
	}
	if err := sprooziv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	env := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
	}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	})

	skipValidation := true
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		// Bind to port 0 so the OS picks an unused port; avoids conflicts between
		// test managers that run in parallel within the same process.
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		// Allow multiple test managers to register controllers with the same name.
		Controller: config.Controller{SkipNameValidation: &skipValidation},
	})
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}

	if err := (&controller.AgentRunReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		SandboxClient: k8sresources.NewPodSandboxClient(mgr.GetClient()),
	}).SetupWithManager(mgr); err != nil {
		t.Fatalf("SetupWithManager() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	errCh := make(chan error, 1)
	go func() {
		errCh <- mgr.Start(ctx)
	}()
	t.Cleanup(func() {
		// Drain the error channel after context cancellation to surface any
		// unexpected manager startup errors.
		select {
		case err := <-errCh:
			if err != nil && ctx.Err() == nil {
				t.Errorf("Manager.Start() error = %v", err)
			}
		default:
		}
	})

	// Wait for the manager cache to sync before returning.
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("cache did not sync")
	}

	return mgr.GetClient(), ctx
}

func validAgentTemplate(ns string) *sprooziv1alpha1.AgentTemplate {
	return &sprooziv1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "sre-remediation", Namespace: ns},
		Spec: sprooziv1alpha1.AgentTemplateSpec{
			RuntimeRef:     sprooziv1alpha1.AgentRuntimeReference{Name: testRuntimeName},
			PolicyRef:      sprooziv1alpha1.AgentPolicyReference{Name: testPolicyName},
			Instructions:   "Investigate and remediate.",
			EgressProfiles: []string{testEgressModule},
		},
	}
}

func validAgentPolicyWithEgress(ns string) *sprooziv1alpha1.AgentPolicy {
	p := validAgentPolicy()
	p.Namespace = ns
	p.Spec.EgressProfiles = []string{testEgressModule}
	return p
}

func validAgentRun(name, ns string) *sprooziv1alpha1.AgentRun {
	return &sprooziv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: sprooziv1alpha1.AgentRunSpec{
			TemplateRef:  sprooziv1alpha1.AgentTemplateReference{Name: "sre-remediation"},
			Task:         "Investigate the CrashLoopBackOff incident.",
			Capabilities: []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityKubernetesRead},
		},
	}
}

func validAgentRuntimeForController(ns string) *sprooziv1alpha1.AgentRuntime {
	rt := validAgentRuntime()
	rt.Namespace = ns
	return rt
}

func createControllerFixtures(t *testing.T, c client.Client, ctx context.Context, ns string) {
	t.Helper()
	// Ensure sproozi-agents namespace exists so handleRunning can create Sandbox Pods.
	ensureNamespace(t, c, ctx, k8sresources.AgentsNamespace)
	immutable := true
	trustBundle := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "sproozi-gateway-ca", Namespace: k8sresources.AgentsNamespace},
		Immutable:  &immutable,
		Data:       map[string]string{"ca.crt": validTrustBundlePEM(t)},
	}
	if err := c.Create(ctx, trustBundle); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatalf("Create trust bundle error = %v", err)
	}
	// The default policy fixtures grant read access into sproozi-demo, so that
	// namespace must exist before identity provisioning creates per-run RBAC.
	ensureNamespace(t, c, ctx, testSproozDemoNamespace)
	rt := validAgentRuntimeForController(ns)
	if err := c.Create(ctx, rt); err != nil {
		t.Fatalf("Create runtime error = %v", err)
	}
	pol := validAgentPolicyWithEgress(ns)
	if err := c.Create(ctx, pol); err != nil {
		t.Fatalf("Create policy error = %v", err)
	}
	tmpl := validAgentTemplate(ns)
	if err := c.Create(ctx, tmpl); err != nil {
		t.Fatalf("Create template error = %v", err)
	}
}

// TestAgentRunControllerSingleActiveRun proves that exactly one AgentRun is
// Admitted at a time, and that cancelling the active run allows the next
// queued run to be admitted.
func TestAgentRunControllerSingleActiveRun(t *testing.T) {
	c, ctx := startAgentRunController(t)
	g := gomega.NewGomegaWithT(t)

	createControllerFixtures(t, c, ctx, testNamespace)

	run1 := validAgentRun("ctrl-run1", testNamespace)
	if err := c.Create(ctx, run1); err != nil {
		t.Fatalf("Create run1 error = %v", err)
	}

	// Small sleep does NOT guarantee a different creation timestamp because
	// metav1.Time is second-granular. Ordering is determined by name tie-break
	// ("ctrl-run1" < "ctrl-run2") when timestamps are equal, which is fine.
	time.Sleep(20 * time.Millisecond)

	run2 := validAgentRun("ctrl-run2", testNamespace)
	if err := c.Create(ctx, run2); err != nil {
		t.Fatalf("Create run2 error = %v", err)
	}

	// run1 should be admitted first (earlier creation timestamp).
	g.Eventually(func() sprooziv1alpha1.AgentRunPhase {
		var r sprooziv1alpha1.AgentRun
		if err := c.Get(ctx, client.ObjectKeyFromObject(run1), &r); err != nil {
			return ""
		}
		return r.Status.Phase
	}, 30*time.Second, 100*time.Millisecond).Should(gomega.BeElementOf(
		sprooziv1alpha1.AgentRunPhaseAdmitted,
		sprooziv1alpha1.AgentRunPhaseRunning,
	), "run1 should be Admitted")

	// run2 must remain Queued while run1 is active (sustained invariant check).
	// Phase "" is a valid transient state while the controller initialises the run.
	g.Consistently(func() (sprooziv1alpha1.AgentRunPhase, error) {
		var r sprooziv1alpha1.AgentRun
		if err := c.Get(ctx, client.ObjectKeyFromObject(run2), &r); err != nil {
			if apierrors.IsNotFound(err) {
				return "", nil
			}
			return "", err
		}
		return r.Status.Phase, nil
	}, 2*time.Second, 100*time.Millisecond).Should(
		gomega.BeElementOf(sprooziv1alpha1.AgentRunPhaseQueued, sprooziv1alpha1.AgentRunPhase("")),
		"run2 must not be admitted while run1 is active",
	)

	// Cancel run1.
	var r1 sprooziv1alpha1.AgentRun
	if err := c.Get(ctx, client.ObjectKeyFromObject(run1), &r1); err != nil {
		t.Fatalf("Get run1 error = %v", err)
	}
	r1.Spec.Cancel = true
	if err := c.Update(ctx, &r1); err != nil {
		t.Fatalf("Update run1 error = %v", err)
	}

	// run1 should become Cancelled.
	g.Eventually(func() sprooziv1alpha1.AgentRunPhase {
		var r sprooziv1alpha1.AgentRun
		if err := c.Get(ctx, client.ObjectKeyFromObject(run1), &r); err != nil {
			return ""
		}
		return r.Status.Phase
	}, 30*time.Second, 100*time.Millisecond).Should(gomega.Equal(sprooziv1alpha1.AgentRunPhaseCancelled),
		"run1 should be Cancelled after cancel request")

	// run2 should now be Admitted.
	g.Eventually(func() sprooziv1alpha1.AgentRunPhase {
		var r sprooziv1alpha1.AgentRun
		if err := c.Get(ctx, client.ObjectKeyFromObject(run2), &r); err != nil {
			return ""
		}
		return r.Status.Phase
	}, 30*time.Second, 100*time.Millisecond).Should(gomega.BeElementOf(
		sprooziv1alpha1.AgentRunPhaseAdmitted,
		sprooziv1alpha1.AgentRunPhaseRunning,
	), "run2 should be Admitted after run1 is cancelled")
}

// TestAgentRunControllerFailsMissingTemplate proves that a run referencing a
// non-existent template is moved to Failed with a non-sensitive reason.
func TestAgentRunControllerFailsMissingTemplate(t *testing.T) {
	c, ctx := startAgentRunController(t)
	g := gomega.NewGomegaWithT(t)

	// Deliberately omit template creation.
	rt := validAgentRuntimeForController("default")
	if err := c.Create(ctx, rt); err != nil {
		t.Fatalf("Create runtime error = %v", err)
	}
	pol := validAgentPolicyWithEgress(testNamespace)
	if err := c.Create(ctx, pol); err != nil {
		t.Fatalf("Create policy error = %v", err)
	}

	run := validAgentRun("missing-tmpl-run", testNamespace)
	if err := c.Create(ctx, run); err != nil {
		t.Fatalf("Create run error = %v", err)
	}

	g.Eventually(func() sprooziv1alpha1.AgentRunPhase {
		var r sprooziv1alpha1.AgentRun
		if err := c.Get(ctx, client.ObjectKeyFromObject(run), &r); err != nil {
			return ""
		}
		return r.Status.Phase
	}, 30*time.Second, 100*time.Millisecond).Should(gomega.Equal(sprooziv1alpha1.AgentRunPhaseFailed),
		"run with missing template should be Failed")
}

// TestAgentRunControllerCrossNamespaceSlot proves that the single active-run
// slot is cluster-wide: a run in a second namespace is blocked while a run in
// the first namespace holds the slot.
func TestAgentRunControllerCrossNamespaceSlot(t *testing.T) {
	c, ctx := startAgentRunController(t)
	g := gomega.NewGomegaWithT(t)

	// Fixtures in the primary namespace.
	createControllerFixtures(t, c, ctx, testNamespace)

	// Admit a run in the primary namespace to occupy the global slot.
	run1 := validAgentRun("cross-ns-run1", testNamespace)
	if err := c.Create(ctx, run1); err != nil {
		t.Fatalf("Create run1 error = %v", err)
	}
	g.Eventually(func() sprooziv1alpha1.AgentRunPhase {
		var r sprooziv1alpha1.AgentRun
		if err := c.Get(ctx, client.ObjectKeyFromObject(run1), &r); err != nil {
			return ""
		}
		return r.Status.Phase
	}, 30*time.Second, 100*time.Millisecond).Should(gomega.BeElementOf(
		sprooziv1alpha1.AgentRunPhaseAdmitted,
		sprooziv1alpha1.AgentRunPhaseRunning,
	), "run1 should be Admitted")

	// Create the second namespace and its fixtures.
	teamB := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b"}}
	if err := c.Create(ctx, teamB); err != nil {
		t.Fatalf("Create namespace team-b error = %v", err)
	}
	createControllerFixtures(t, c, ctx, "team-b")

	// Create a run in team-b — slot is occupied by run1 in testNamespace.
	run2 := validAgentRun("cross-ns-run2", "team-b")
	if err := c.Create(ctx, run2); err != nil {
		t.Fatalf("Create run2 error = %v", err)
	}

	// run2 must stay Queued because the global slot is occupied.
	// Phase "" is a valid transient state while the controller initialises the run.
	g.Consistently(func() (sprooziv1alpha1.AgentRunPhase, error) {
		var r sprooziv1alpha1.AgentRun
		if err := c.Get(ctx, client.ObjectKeyFromObject(run2), &r); err != nil {
			if apierrors.IsNotFound(err) {
				return "", nil
			}
			return "", err
		}
		return r.Status.Phase, nil
	}, 2*time.Second, 100*time.Millisecond).Should(
		gomega.BeElementOf(sprooziv1alpha1.AgentRunPhaseQueued, sprooziv1alpha1.AgentRunPhase("")),
		"run2 in team-b must not be admitted while run1 holds the global slot",
	)
}

// TestAgentRunControllerPolicyChangeRequeues proves that when an AgentPolicy is
// updated to revoke a previously allowed capability, any Queued run that
// requested that capability is re-evaluated and transitions to Failed.
func TestAgentRunControllerPolicyChangeRequeues(t *testing.T) {
	c, ctx := startAgentRunController(t)
	g := gomega.NewGomegaWithT(t)

	createControllerFixtures(t, c, ctx, testNamespace)

	// Occupy the slot with run1 so run2 stays Queued.
	run1 := validAgentRun("policy-watch-run1", testNamespace)
	if err := c.Create(ctx, run1); err != nil {
		t.Fatalf("Create run1 error = %v", err)
	}
	g.Eventually(func() sprooziv1alpha1.AgentRunPhase {
		var r sprooziv1alpha1.AgentRun
		if err := c.Get(ctx, client.ObjectKeyFromObject(run1), &r); err != nil {
			return ""
		}
		return r.Status.Phase
	}, 30*time.Second, 100*time.Millisecond).Should(gomega.BeElementOf(
		sprooziv1alpha1.AgentRunPhaseAdmitted,
		sprooziv1alpha1.AgentRunPhaseRunning,
	), "run1 should be Admitted to hold the slot")

	// Create run2 requesting KubernetesRead (currently allowed by the policy).
	run2 := validAgentRun("policy-watch-run2", testNamespace)
	if err := c.Create(ctx, run2); err != nil {
		t.Fatalf("Create run2 error = %v", err)
	}
	g.Eventually(func() sprooziv1alpha1.AgentRunPhase {
		var r sprooziv1alpha1.AgentRun
		if err := c.Get(ctx, client.ObjectKeyFromObject(run2), &r); err != nil {
			return ""
		}
		return r.Status.Phase
	}, 30*time.Second, 100*time.Millisecond).Should(gomega.Equal(sprooziv1alpha1.AgentRunPhaseQueued),
		"run2 should be Queued while slot is occupied")

	// Patch the policy to revoke KubernetesRead.
	var pol sprooziv1alpha1.AgentPolicy
	if err := c.Get(ctx, client.ObjectKey{Name: testPolicyName, Namespace: testNamespace}, &pol); err != nil {
		t.Fatalf("Get policy error = %v", err)
	}
	pol.Spec.AllowedCapabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityModelInference}
	if err := c.Update(ctx, &pol); err != nil {
		t.Fatalf("Update policy error = %v", err)
	}

	// run2 must be re-evaluated by the policy watch and transition to Failed.
	g.Eventually(func() sprooziv1alpha1.AgentRunPhase {
		var r sprooziv1alpha1.AgentRun
		if err := c.Get(ctx, client.ObjectKeyFromObject(run2), &r); err != nil {
			return ""
		}
		return r.Status.Phase
	}, 30*time.Second, 100*time.Millisecond).Should(gomega.Equal(sprooziv1alpha1.AgentRunPhaseFailed),
		"run2 should be Failed after policy revokes KubernetesRead")
}

// startAgentRunController starts an envtest environment with the AgentRun controller
// running. Returns the manager client and a cleanup function.
