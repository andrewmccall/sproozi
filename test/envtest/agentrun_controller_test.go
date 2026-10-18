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
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/controller"
	k8sresources "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
)

const (
	// controllerTimeout bounds each wait for a controller transition.
	controllerTimeout = 30 * time.Second
	// slotHoldDuration is how long a queued run is watched to stay queued
	// while another run holds the single active slot.
	slotHoldDuration = 2 * time.Second
	pollInterval     = 100 * time.Millisecond
)

// The active-run slot is cluster-wide, so controller tests share one manager,
// never call t.Parallel and release every run they create before they end.
var (
	managerOnce   sync.Once
	managerClient client.Client
	managerErr    error
	stopManager   = func() {}
)

// startAgentRunController starts the shared AgentRun controller on first use.
// It returns the manager client and a namespace private to the calling test;
// runs left in that namespace are cancelled when the test ends.
func startAgentRunController(t *testing.T) (client.Client, context.Context, string) {
	t.Helper()
	managerOnce.Do(func() { managerClient, managerErr = runAgentRunController() })
	if managerErr != nil {
		t.Fatalf("start AgentRun controller: %v", managerErr)
	}
	return managerClient, t.Context(), controllerNamespace(t)
}

func runAgentRunController() (client.Client, error) {
	log.SetLogger(zap.New(zap.UseDevMode(true)))
	mgr, err := ctrl.NewManager(testConfig, ctrl.Options{
		Scheme: testScheme,
		// Bind to port 0 so the OS picks unused ports.
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
	})
	if err != nil {
		return nil, err
	}
	if err := (&controller.AgentRunReconciler{
		Client:        mgr.GetClient(),
		APIReader:     mgr.GetAPIReader(),
		SandboxClient: k8sresources.NewPodSandboxClient(mgr.GetClient(), mgr.GetAPIReader()),
	}).SetupWithManager(mgr); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := mgr.Start(ctx); err != nil {
			log.Log.Error(err, "Failed to run manager")
		}
	}()
	stopManager = func() { cancel(); <-done }
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		stopManager()
		return nil, errors.New("cache did not sync")
	}
	return mgr.GetClient(), nil
}

func stopAgentRunController() { stopManager() }

// controllerNamespace returns a fresh namespace whose runs are cancelled when
// the test ends. Cleanup waits until each is terminal, so the next controller
// test starts with the single active slot free.
func controllerNamespace(t *testing.T) string {
	t.Helper()
	ns := newNamespace(t)
	t.Cleanup(func() {
		// t.Context is already cancelled while cleanup runs.
		ctx := context.Background()
		g := gomega.NewGomegaWithT(t)
		g.Eventually(func() error {
			var runs sprooziv1alpha1.AgentRunList
			if err := testClient.List(ctx, &runs, client.InNamespace(ns)); err != nil {
				return err
			}
			for i := range runs.Items {
				run := &runs.Items[i]
				if controller.IsTerminal(run.Status.Phase) {
					continue
				}
				if !run.Spec.Cancel {
					run.Spec.Cancel = true
					if err := testClient.Update(ctx, run); err != nil {
						return err
					}
				}
				return fmt.Errorf("run %s is still %q", run.Name, run.Status.Phase)
			}
			return nil
		}, controllerTimeout, pollInterval).Should(gomega.Succeed(), "runs in %s were not released", ns)
	})
	return ns
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
	p := validAgentPolicy(ns)
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
	if err := c.Create(ctx, validAgentRuntime(ns)); err != nil {
		t.Fatalf("Create runtime error = %v", err)
	}
	if err := c.Create(ctx, validAgentPolicyWithEgress(ns)); err != nil {
		t.Fatalf("Create policy error = %v", err)
	}
	if err := c.Create(ctx, validAgentTemplate(ns)); err != nil {
		t.Fatalf("Create template error = %v", err)
	}
}

// waitForPhase waits until the run reaches one of the given phases.
func waitForPhase(g *gomega.WithT, c client.Client, ctx context.Context, run *sprooziv1alpha1.AgentRun,
	description string, phases ...sprooziv1alpha1.AgentRunPhase) {
	g.Eventually(func() sprooziv1alpha1.AgentRunPhase {
		var r sprooziv1alpha1.AgentRun
		if err := c.Get(ctx, client.ObjectKeyFromObject(run), &r); err != nil {
			return ""
		}
		return r.Status.Phase
	}, controllerTimeout, pollInterval).Should(gomega.BeElementOf(phases), description)
}

// TestAgentRunControllerCrossNamespaceSlot proves that the single active-run
// slot is cluster-wide: a run in a second namespace is blocked while a run in
// the first namespace holds the slot.
func TestAgentRunControllerCrossNamespaceSlot(t *testing.T) {
	c, ctx, ns := startAgentRunController(t)
	g := gomega.NewGomegaWithT(t)

	createControllerFixtures(t, c, ctx, ns)

	// Admit a run in the primary namespace to occupy the global slot.
	run1 := validAgentRun("cross-ns-run1", ns)
	if err := c.Create(ctx, run1); err != nil {
		t.Fatalf("Create run1 error = %v", err)
	}
	waitForPhase(g, c, ctx, run1, "run1 should be Admitted",
		sprooziv1alpha1.AgentRunPhaseAdmitted, sprooziv1alpha1.AgentRunPhaseRunning)

	// A second namespace with its own fixtures.
	teamB := controllerNamespace(t)
	createControllerFixtures(t, c, ctx, teamB)

	// Create a run in the second namespace while run1 holds the slot.
	run2 := validAgentRun("cross-ns-run2", teamB)
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
	}, slotHoldDuration, pollInterval).Should(
		gomega.BeElementOf(sprooziv1alpha1.AgentRunPhaseQueued, sprooziv1alpha1.AgentRunPhase("")),
		"run2 in a second namespace must not be admitted while run1 holds the global slot",
	)
}

// TestAgentRunControllerPolicyChangeRequeues proves that when an AgentPolicy is
// updated to revoke a previously allowed capability, any Queued run that
// requested that capability is re-evaluated and transitions to Failed.
func TestAgentRunControllerPolicyChangeRequeues(t *testing.T) {
	c, ctx, ns := startAgentRunController(t)
	g := gomega.NewGomegaWithT(t)

	createControllerFixtures(t, c, ctx, ns)

	// Occupy the slot with run1 so run2 stays Queued.
	run1 := validAgentRun("policy-watch-run1", ns)
	if err := c.Create(ctx, run1); err != nil {
		t.Fatalf("Create run1 error = %v", err)
	}
	waitForPhase(g, c, ctx, run1, "run1 should be Admitted to hold the slot",
		sprooziv1alpha1.AgentRunPhaseAdmitted, sprooziv1alpha1.AgentRunPhaseRunning)

	// Create run2 requesting KubernetesRead (currently allowed by the policy).
	run2 := validAgentRun("policy-watch-run2", ns)
	if err := c.Create(ctx, run2); err != nil {
		t.Fatalf("Create run2 error = %v", err)
	}
	waitForPhase(g, c, ctx, run2, "run2 should be Queued while slot is occupied", sprooziv1alpha1.AgentRunPhaseQueued)

	// Patch the policy to revoke KubernetesRead.
	var pol sprooziv1alpha1.AgentPolicy
	if err := c.Get(ctx, client.ObjectKey{Name: testPolicyName, Namespace: ns}, &pol); err != nil {
		t.Fatalf("Get policy error = %v", err)
	}
	pol.Spec.AllowedCapabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityModelInference}
	if err := c.Update(ctx, &pol); err != nil {
		t.Fatalf("Update policy error = %v", err)
	}

	// run2 must be re-evaluated by the policy watch and transition to Failed.
	waitForPhase(g, c, ctx, run2, "run2 should be Failed after policy revokes KubernetesRead",
		sprooziv1alpha1.AgentRunPhaseFailed)
}
