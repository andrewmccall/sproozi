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
	"testing"
	"time"

	"github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/config"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/controller"
	gatewayk8s "github.com/andrewmccall/sproozi/internal/gateway/kubernetes"
	k8sresources "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
)

const (
	// controllerTimeout bounds each wait for a controller transition.
	controllerTimeout = 30 * time.Second
	// capHoldDuration is how long a queued run is watched to stay queued
	// while a cap is full.
	capHoldDuration = 2 * time.Second
	// promptWake bounds how long a queued run may wait for freed capacity. It
	// is well under the controller's 10s fallback poll, so passing proves a
	// watch woke the run.
	promptWake   = 3 * time.Second
	pollInterval = 100 * time.Millisecond
)

// Admission caps are cluster-wide, so controller tests share one manager,
// never call t.Parallel and release every run they create before they end.
// The manager is restarted only when a test asks for a different cluster cap.
var (
	managerRunning bool
	managerCap     int
	managerClient  client.Client
	stopManager    = func() {}
)

// startAgentRunController starts the shared AgentRun controller with no
// cluster cap. It returns the manager client and a namespace private to the
// calling test; runs left in that namespace are cancelled when the test ends.
func startAgentRunController(t *testing.T) (client.Client, context.Context, string) {
	t.Helper()
	return startAgentRunControllerWithCap(t, 0)
}

// startAgentRunControllerWithCap is startAgentRunController with the
// controller's --max-active-runs set to maxActiveRuns.
func startAgentRunControllerWithCap(t *testing.T, maxActiveRuns int) (client.Client, context.Context, string) {
	t.Helper()
	if !managerRunning || managerCap != maxActiveRuns {
		stopAgentRunController()
		c, err := runAgentRunController(maxActiveRuns)
		if err != nil {
			t.Fatalf("start AgentRun controller: %v", err)
		}
		managerClient, managerCap, managerRunning = c, maxActiveRuns, true
	}
	return managerClient, t.Context(), controllerNamespace(t)
}

func runAgentRunController(maxActiveRuns int) (client.Client, error) {
	log.SetLogger(zap.New(zap.UseDevMode(true)))
	mgr, err := ctrl.NewManager(testConfig, ctrl.Options{
		Scheme: testScheme,
		// Bind to port 0 so the OS picks unused ports.
		Metrics:                metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0",
		// A restarted manager registers the same controller name again.
		Controller: config.Controller{SkipNameValidation: ptr.To(true)},
	})
	if err != nil {
		return nil, err
	}
	if err := (&controller.AgentRunReconciler{
		Client:        mgr.GetClient(),
		APIReader:     mgr.GetAPIReader(),
		SandboxClient: k8sresources.NewPodSandboxClient(mgr.GetClient(), mgr.GetAPIReader()),
		MaxActiveRuns: maxActiveRuns,
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

func stopAgentRunController() {
	stopManager()
	stopManager = func() {}
	managerRunning = false
}

// controllerNamespace returns a fresh namespace whose runs are cancelled when
// the test ends. Cleanup waits until each is terminal, so the next controller
// test starts with no active runs.
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

// runPhase reads a run's phase; a run not yet visible reads as "".
func runPhase(
	c client.Client, ctx context.Context, run *sprooziv1alpha1.AgentRun,
) (sprooziv1alpha1.AgentRunPhase, error) {
	var r sprooziv1alpha1.AgentRun
	if err := c.Get(ctx, client.ObjectKeyFromObject(run), &r); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return r.Status.Phase, nil
}

// expectStaysQueued checks the run is not admitted for capHoldDuration.
// Phase "" is a valid transient state while the controller initialises the run.
func expectStaysQueued(
	g *gomega.WithT, c client.Client, ctx context.Context, run *sprooziv1alpha1.AgentRun, description string,
) {
	g.Consistently(func() (sprooziv1alpha1.AgentRunPhase, error) {
		return runPhase(c, ctx, run)
	}, capHoldDuration, pollInterval).Should(
		gomega.BeElementOf(sprooziv1alpha1.AgentRunPhaseQueued, sprooziv1alpha1.AgentRunPhase("")), description)
}

// expectAdmittedPromptly checks the run is admitted within promptWake.
func expectAdmittedPromptly(
	g *gomega.WithT, c client.Client, ctx context.Context, run *sprooziv1alpha1.AgentRun, description string,
) {
	g.Eventually(func() (sprooziv1alpha1.AgentRunPhase, error) {
		return runPhase(c, ctx, run)
	}, promptWake, pollInterval).Should(
		gomega.BeElementOf(sprooziv1alpha1.AgentRunPhaseAdmitted, sprooziv1alpha1.AgentRunPhaseRunning), description)
}

func createRun(t *testing.T, c client.Client, ctx context.Context, run *sprooziv1alpha1.AgentRun) {
	t.Helper()
	if err := c.Create(ctx, run); err != nil {
		t.Fatalf("Create %s error = %v", run.Name, err)
	}
}

func cancelRun(t *testing.T, c client.Client, ctx context.Context, run *sprooziv1alpha1.AgentRun) {
	t.Helper()
	var latest sprooziv1alpha1.AgentRun
	if err := c.Get(ctx, client.ObjectKeyFromObject(run), &latest); err != nil {
		t.Fatalf("Get %s error = %v", run.Name, err)
	}
	latest.Spec.Cancel = true
	if err := c.Update(ctx, &latest); err != nil {
		t.Fatalf("Cancel %s error = %v", run.Name, err)
	}
}

// setPolicyCap sets maxActiveRuns on the fixture policy.
func setPolicyCap(t *testing.T, c client.Client, ctx context.Context, ns string, maxActiveRuns *int32) {
	t.Helper()
	var pol sprooziv1alpha1.AgentPolicy
	if err := c.Get(ctx, client.ObjectKey{Name: testPolicyName, Namespace: ns}, &pol); err != nil {
		t.Fatalf("Get policy error = %v", err)
	}
	pol.Spec.MaxActiveRuns = maxActiveRuns
	if err := c.Update(ctx, &pol); err != nil {
		t.Fatalf("Update policy error = %v", err)
	}
}

// createPolicyAndTemplate adds a second policy and a template of the same
// name that references it.
func createPolicyAndTemplate(
	t *testing.T, c client.Client, ctx context.Context, ns, name string, maxActiveRuns *int32,
) {
	t.Helper()
	pol := validAgentPolicyWithEgress(ns)
	pol.Name = name
	pol.Spec.MaxActiveRuns = maxActiveRuns
	if err := c.Create(ctx, pol); err != nil {
		t.Fatalf("Create policy %s error = %v", name, err)
	}
	tmpl := validAgentTemplate(ns)
	tmpl.Name = name
	tmpl.Spec.PolicyRef.Name = name
	if err := c.Create(ctx, tmpl); err != nil {
		t.Fatalf("Create template %s error = %v", name, err)
	}
}

// TestAgentRunControllerNoCapAdmitsConcurrently proves the default install,
// with no cluster or policy cap, runs two AgentRuns at once and records the
// admitting policy on each.
func TestAgentRunControllerNoCapAdmitsConcurrently(t *testing.T) {
	c, ctx, ns := startAgentRunController(t)
	g := gomega.NewGomegaWithT(t)
	createControllerFixtures(t, c, ctx, ns)

	run1 := validAgentRun("no-cap-run1", ns)
	run2 := validAgentRun("no-cap-run2", ns)
	createRun(t, c, ctx, run1)
	createRun(t, c, ctx, run2)
	for _, run := range []*sprooziv1alpha1.AgentRun{run1, run2} {
		waitForPhase(g, c, ctx, run, run.Name+" should be active without a cap",
			sprooziv1alpha1.AgentRunPhaseAdmitted, sprooziv1alpha1.AgentRunPhaseRunning)
	}
	for _, run := range []*sprooziv1alpha1.AgentRun{run1, run2} {
		var got sprooziv1alpha1.AgentRun
		if err := c.Get(ctx, client.ObjectKeyFromObject(run), &got); err != nil {
			t.Fatal(err)
		}
		if !controller.IsActive(got.Status.Phase) {
			t.Fatalf("%s phase = %q, want both runs active at once", run.Name, got.Status.Phase)
		}
		if got.Status.PolicyName != testPolicyName {
			t.Fatalf("%s status.policyName = %q, want %q", run.Name, got.Status.PolicyName, testPolicyName)
		}
	}
}

// TestAgentRunControllerCrossNamespaceCap proves the cluster cap counts runs
// in every namespace: with --max-active-runs=1 a run in a second namespace is
// blocked while a run in the first is active.
func TestAgentRunControllerCrossNamespaceCap(t *testing.T) {
	c, ctx, ns := startAgentRunControllerWithCap(t, 1)
	g := gomega.NewGomegaWithT(t)
	createControllerFixtures(t, c, ctx, ns)

	run1 := validAgentRun("cross-ns-run1", ns)
	createRun(t, c, ctx, run1)
	waitForPhase(g, c, ctx, run1, "run1 should be Admitted",
		sprooziv1alpha1.AgentRunPhaseAdmitted, sprooziv1alpha1.AgentRunPhaseRunning)

	teamB := controllerNamespace(t)
	createControllerFixtures(t, c, ctx, teamB)
	run2 := validAgentRun("cross-ns-run2", teamB)
	createRun(t, c, ctx, run2)
	expectStaysQueued(g, c, ctx, run2, "run2 in a second namespace must not be admitted while run1 fills the cluster cap")
}

// TestAgentRunControllerGlobalCapWakesQueuedRun proves a third run waits at
// --max-active-runs=2 and is admitted promptly, not at the fallback poll, when
// an active run finishes.
func TestAgentRunControllerGlobalCapWakesQueuedRun(t *testing.T) {
	c, ctx, ns := startAgentRunControllerWithCap(t, 2)
	g := gomega.NewGomegaWithT(t)
	createControllerFixtures(t, c, ctx, ns)

	run1 := validAgentRun("global-cap-run1", ns)
	run2 := validAgentRun("global-cap-run2", ns)
	createRun(t, c, ctx, run1)
	createRun(t, c, ctx, run2)
	for _, run := range []*sprooziv1alpha1.AgentRun{run1, run2} {
		waitForPhase(g, c, ctx, run, run.Name+" should be active under cap 2",
			sprooziv1alpha1.AgentRunPhaseAdmitted, sprooziv1alpha1.AgentRunPhaseRunning)
	}
	run3 := validAgentRun("global-cap-run3", ns)
	createRun(t, c, ctx, run3)
	expectStaysQueued(g, c, ctx, run3, "run3 must wait while two runs fill cap 2")

	cancelRun(t, c, ctx, run1)
	expectAdmittedPromptly(g, c, ctx, run3, "run3 should be admitted as soon as run1 finishes")
}

// TestAgentRunControllerPolicyCapDoesNotBlockOtherPolicies proves a queued run
// whose policy is full does not hold back a later run under another policy.
func TestAgentRunControllerPolicyCapDoesNotBlockOtherPolicies(t *testing.T) {
	c, ctx, ns := startAgentRunController(t)
	g := gomega.NewGomegaWithT(t)
	createControllerFixtures(t, c, ctx, ns)
	setPolicyCap(t, c, ctx, ns, ptr.To(int32(1)))
	createPolicyAndTemplate(t, c, ctx, ns, "policy-b", ptr.To(int32(1)))

	a1 := validAgentRun("policy-a-run1", ns)
	createRun(t, c, ctx, a1)
	waitForPhase(g, c, ctx, a1, "a1 should fill policy sre cap 1",
		sprooziv1alpha1.AgentRunPhaseAdmitted, sprooziv1alpha1.AgentRunPhaseRunning)
	a2 := validAgentRun("policy-a-run2", ns)
	createRun(t, c, ctx, a2)
	waitForPhase(g, c, ctx, a2, "a2 should be Queued behind policy sre cap 1", sprooziv1alpha1.AgentRunPhaseQueued)

	b1 := validAgentRun("policy-b-run1", ns)
	b1.Spec.TemplateRef.Name = "policy-b"
	createRun(t, c, ctx, b1)
	waitForPhase(g, c, ctx, b1, "b1 should be admitted while a2 waits at the queue head",
		sprooziv1alpha1.AgentRunPhaseAdmitted, sprooziv1alpha1.AgentRunPhaseRunning)
	if phase, err := runPhase(c, ctx, a2); err != nil || phase != sprooziv1alpha1.AgentRunPhaseQueued {
		t.Fatalf("a2 phase = %q (%v), want still Queued", phase, err)
	}
}

// TestAgentRunControllerRaisingPolicyCapAdmitsPromptly proves a policy cap
// change wakes the runs waiting under it.
func TestAgentRunControllerRaisingPolicyCapAdmitsPromptly(t *testing.T) {
	c, ctx, ns := startAgentRunController(t)
	g := gomega.NewGomegaWithT(t)
	createControllerFixtures(t, c, ctx, ns)
	setPolicyCap(t, c, ctx, ns, ptr.To(int32(1)))

	run1 := validAgentRun("raise-cap-run1", ns)
	createRun(t, c, ctx, run1)
	waitForPhase(g, c, ctx, run1, "run1 should fill policy cap 1",
		sprooziv1alpha1.AgentRunPhaseAdmitted, sprooziv1alpha1.AgentRunPhaseRunning)
	run2 := validAgentRun("raise-cap-run2", ns)
	createRun(t, c, ctx, run2)
	expectStaysQueued(g, c, ctx, run2, "run2 must wait at policy cap 1")

	setPolicyCap(t, c, ctx, ns, ptr.To(int32(2)))
	expectAdmittedPromptly(g, c, ctx, run2, "run2 should be admitted as soon as the cap is raised")
}

// TestAgentRunControllerPolicyChangeRequeues proves that when an AgentPolicy is
// updated to revoke a previously allowed capability, any Queued run that
// requested that capability is re-evaluated and transitions to Failed.
func TestAgentRunControllerPolicyChangeRequeues(t *testing.T) {
	c, ctx, ns := startAgentRunController(t)
	g := gomega.NewGomegaWithT(t)

	createControllerFixtures(t, c, ctx, ns)
	setPolicyCap(t, c, ctx, ns, ptr.To(int32(1)))

	// Fill the policy cap with run1 so run2 stays Queued.
	run1 := validAgentRun("policy-watch-run1", ns)
	createRun(t, c, ctx, run1)
	waitForPhase(g, c, ctx, run1, "run1 should be Admitted to fill the policy cap",
		sprooziv1alpha1.AgentRunPhaseAdmitted, sprooziv1alpha1.AgentRunPhaseRunning)

	// Create run2 requesting KubernetesRead (currently allowed by the policy).
	run2 := validAgentRun("policy-watch-run2", ns)
	createRun(t, c, ctx, run2)
	waitForPhase(g, c, ctx, run2, "run2 should be Queued at the policy cap", sprooziv1alpha1.AgentRunPhaseQueued)

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

// TestAgentRunControllerRepointedTemplateFailsRunningRun proves that
// re-pointing a running run's template to another AgentPolicy fails the run
// with PolicyChanged, releases its sandbox, and that the gateway refuses the
// run's identity rather than grant it the new policy's permissions.
func TestAgentRunControllerRepointedTemplateFailsRunningRun(t *testing.T) {
	c, ctx, ns := startAgentRunController(t)
	g := gomega.NewGomegaWithT(t)
	createControllerFixtures(t, c, ctx, ns)
	createPolicyAndTemplate(t, c, ctx, ns, "other", nil)

	run := validAgentRun("repointed-run", ns)
	createRun(t, c, ctx, run)
	var running sprooziv1alpha1.AgentRun
	g.Eventually(func() string {
		if err := c.Get(ctx, client.ObjectKeyFromObject(run), &running); err != nil {
			return ""
		}
		return running.Status.Identity.SandboxName
	}, controllerTimeout, pollInterval).ShouldNot(gomega.BeEmpty(), "run should reach Running with a sandbox")
	sandboxKey := client.ObjectKey{Namespace: k8sresources.AgentsNamespace, Name: running.Status.Identity.SandboxName}

	var sa corev1.ServiceAccount
	if err := testClient.Get(ctx, client.ObjectKey{Namespace: k8sresources.AgentsNamespace,
		Name: running.Status.Identity.ServiceAccountName}, &sa); err != nil {
		t.Fatalf("Get run ServiceAccount error = %v", err)
	}
	identity := gatewayk8s.ServiceAccountIdentity{Namespace: sa.Namespace, Name: sa.Name, UID: string(sa.UID)}
	authenticator := gatewayk8s.NewAuthenticator(nil, "", testClient, k8sresources.AgentsNamespace)
	if _, err := authenticator.Find(ctx, identity); err != nil {
		t.Fatalf("gateway Find before re-pointing error = %v", err)
	}

	var tmpl sprooziv1alpha1.AgentTemplate
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: run.Spec.TemplateRef.Name}, &tmpl); err != nil {
		t.Fatalf("Get template error = %v", err)
	}
	tmpl.Spec.PolicyRef.Name = "other"
	if err := c.Update(ctx, &tmpl); err != nil {
		t.Fatalf("Update template error = %v", err)
	}
	// Whether or not the controller has failed the run yet, the gateway must
	// not grant it: PolicyChanged before, not Running (or no identity) after.
	if grant, err := authenticator.Find(ctx, identity); err == nil {
		t.Fatalf("gateway granted the re-pointed run under AgentPolicy %s", grant.Policy.Name)
	}

	waitForPhase(g, c, ctx, run, "run should fail after its template is re-pointed", sprooziv1alpha1.AgentRunPhaseFailed)
	var failed sprooziv1alpha1.AgentRun
	if err := c.Get(ctx, client.ObjectKeyFromObject(run), &failed); err != nil {
		t.Fatal(err)
	}
	ready := apimeta.FindStatusCondition(failed.Status.Conditions, "Ready")
	want := "AgentTemplate sre-remediation now references AgentPolicy other, " +
		"but this run was admitted under AgentPolicy " + testPolicyName + "; submit a new run"
	if ready == nil || ready.Reason != "PolicyChanged" || ready.Message != want {
		t.Fatalf("Ready condition = %#v, want PolicyChanged %q", ready, want)
	}
	g.Eventually(func() bool {
		var pod corev1.Pod
		err := testClient.Get(ctx, sandboxKey, &pod)
		return apierrors.IsNotFound(err) || err == nil && !pod.DeletionTimestamp.IsZero()
	}, controllerTimeout, pollInterval).Should(gomega.BeTrue(), "sandbox Pod should be deleted")
}
