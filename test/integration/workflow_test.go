package e2e_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/utils/ptr"

	corev1 "k8s.io/api/core/v1"

	networkingv1 "k8s.io/api/networking/v1"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"

	clientgoscheme "k8s.io/client-go/kubernetes/scheme"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/audit"
	"github.com/andrewmccall/sproozi/internal/controller"

	k8sresources "github.com/andrewmccall/sproozi/internal/kubernetes"
	"github.com/andrewmccall/sproozi/internal/webhook"
)

const (
	e2eNamespace = "default"
	e2eReceiver  = "alertmanager"
)

type discardWebhookLogger struct{}

func (discardWebhookLogger) LogWebhook(audit.WebhookEvent) {}

func e2eScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := sprooziv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme sproozi: %v", err)
	}
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme core: %v", err)
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme networking: %v", err)
	}
	if err := rbacv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme rbac: %v", err)
	}
	return scheme
}

func e2ePolicy() *sprooziv1alpha1.AgentPolicy {
	return &sprooziv1alpha1.AgentPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "sre",
			Namespace:  e2eNamespace,
			Generation: 1,
		},
		Spec: sprooziv1alpha1.AgentPolicySpec{
			AllowedCapabilities: []sprooziv1alpha1.CapabilityKind{
				sprooziv1alpha1.CapabilityKubernetesRead,
				sprooziv1alpha1.CapabilityGitHubPullRequest,
			},
			KubernetesRead: sprooziv1alpha1.KubernetesReadScope{
				Namespaces: []string{"sproozi-demo"},
				Resources:  []sprooziv1alpha1.KubernetesReadResource{"deployments", "pods", "events", "pods/log"},
			},
			GitHubPullRequest: sprooziv1alpha1.GitHubPullRequestScope{
				Repositories:        []string{"andrewmccall/home-ops"},
				AllowedBaseBranches: []string{"main"},
			},
			EgressProfiles: []string{"go-modules"},
			Budgets: map[sprooziv1alpha1.CapabilityKind]sprooziv1alpha1.EndpointBudget{
				sprooziv1alpha1.CapabilityModelInference: {
					MaxUnits:      100_000,
					MaxCostMicros: 500_000,
				}},
			ResourceBounds: sprooziv1alpha1.ResourceBounds{
				Max: corev1.ResourceList{
					corev1.ResourceCPU: resource.MustParse("2"),
				},
			},
			MaxExecutionDuration: metav1.Duration{Duration: time.Hour},
			RetentionTTL:         metav1.Duration{Duration: 30 * 24 * time.Hour},
		},
	}
}

func e2eRuntime() *sprooziv1alpha1.AgentRuntime {
	allowPrivilegeEscalation, readOnlyRootFilesystem, runAsNonRoot, automount := false, true, true, false
	return &sprooziv1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "codex",
			Namespace: e2eNamespace,
		},
		Spec: sprooziv1alpha1.AgentRuntimeSpec{
			PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
				AutomountServiceAccountToken: &automount,
				Containers: []corev1.Container{{
					Name:  "agent",
					Image: "ghcr.io/openai/codex-universal@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					Resources: corev1.ResourceRequirements{
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("1"),
							corev1.ResourceMemory: resource.MustParse("1Gi"),
						},
					}, SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: &allowPrivilegeEscalation,
						ReadOnlyRootFilesystem:   &readOnlyRootFilesystem,
						RunAsNonRoot:             &runAsNonRoot,
						Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
				}}, SecurityContext: &corev1.PodSecurityContext{
					RunAsNonRoot:   ptr.To(true),
					RunAsUser:      ptr.To(int64(65532)),
					FSGroup:        ptr.To(int64(65532)),
					SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			}},
			WorkloadContainers: []string{"agent"},
			ClientConfig: sprooziv1alpha1.RuntimeClientConfig{TrustBundleConfigMap: sprooziv1alpha1.RuntimeConfigMapKeyReference{
				Name: "sproozi-gateway-ca", Key: "ca.crt",
			}},

			EphemeralWorkspace: sprooziv1alpha1.EphemeralWorkspaceSpec{
				SizeLimit: resource.MustParse("2Gi"),
			}, GatewayEndpoint: "https://sproozi-gateway.sproozi-system.svc:8443",
		},
	}
}

func e2eTemplate() *sprooziv1alpha1.AgentTemplate {
	return &sprooziv1alpha1.AgentTemplate{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sre-remediation",
			Namespace: e2eNamespace,
			Labels: map[string]string{
				webhook.ReceiverLabel: e2eReceiver,
			},
			Annotations: map[string]string{
				webhook.SecretNameAnnotation: "alertmanager-secret",
			},
		},
		Spec: sprooziv1alpha1.AgentTemplateSpec{
			RuntimeRef:     sprooziv1alpha1.AgentRuntimeReference{Name: "codex"},
			PolicyRef:      sprooziv1alpha1.AgentPolicyReference{Name: "sre"},
			Instructions:   "Investigate the failing workload and open a pull request.",
			EgressProfiles: []string{"go-modules"},
		},
	}
}

func e2eSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "alertmanager-secret",
			Namespace: e2eNamespace,
		},
		Data: map[string][]byte{
			"hmac-key": []byte("webhook-secret"),
		},
	}
}

func e2eClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	return fake.NewClientBuilder().
		WithScheme(e2eScheme(t)).
		WithObjects(objs...).
		WithStatusSubresource(&sprooziv1alpha1.AgentRun{}).
		Build()
}

func e2eObjects() []client.Object {
	return []client.Object{
		e2eRuntime(),
		e2ePolicy(),
		e2eTemplate(),
		e2eSecret(),
	}
}

func listE2ERuns(t *testing.T, c client.Client) []sprooziv1alpha1.AgentRun {
	t.Helper()
	var runs sprooziv1alpha1.AgentRunList
	if err := c.List(context.Background(), &runs); err != nil {
		t.Fatalf("List AgentRuns: %v", err)
	}
	return runs.Items
}

func reconcileRunNTimes(t *testing.T, r *controller.AgentRunReconciler, name string, count int) {
	t.Helper()
	for range count {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{
			NamespacedName: client.ObjectKey{Name: name, Namespace: e2eNamespace},
		}); err != nil {
			t.Fatalf("Reconcile AgentRun %q: %v", name, err)
		}
	}
}

func createRunViaWebhook(t *testing.T, c client.Client, body string) sprooziv1alpha1.AgentRun {
	t.Helper()
	handler := &webhook.Handler{
		Config: webhook.HandlerConfig{
			Client:      c,
			Replay:      webhook.NewInMemoryReplayStore(),
			AuditLogger: discardWebhookLogger{},
		},
	}

	ts := fmt.Sprintf("%d", time.Now().Unix())
	req := httptest.NewRequest(http.MethodPost, "/webhook/"+e2eNamespace+"/"+e2eReceiver, strings.NewReader(body))
	req.Header.Set(webhook.TimestampHeader, ts)
	req.Header.Set(webhook.SignatureHeader, webhook.ComputeSignature(ts, []byte(body), []byte("webhook-secret")))

	resp := httptest.NewRecorder()
	handler.ServeHTTP(resp, req)
	if resp.Code != http.StatusCreated {
		t.Fatalf("expected webhook create 201, got %d body=%s", resp.Code, resp.Body.String())
	}

	runs := listE2ERuns(t, c)
	if len(runs) != 1 {
		t.Fatalf("expected one AgentRun, got %d", len(runs))
	}
	// The fake client does not emulate the API server's UID allocation. Assign
	// one so the same immutable run-identity contract is exercised as in-cluster.
	run := runs[0]
	run.UID = types.UID("e2e-" + run.Name)
	if err := c.Update(context.Background(), &run); err != nil {
		t.Fatalf("assign fake AgentRun UID: %v", err)
	}
	return run
}

func TestWebhookTriggeredRunReachesSucceeded(t *testing.T) {
	c := e2eClient(t, e2eObjects()...)
	run := createRunViaWebhook(t, c, `{
  "id": "evt-100",
  "eventContext": {
    "alertname": "CrashLoopBackOff",
    "namespace": "sproozi-demo"
  }
}`)

	sandbox := &k8sresources.FakeSandboxClient{
		Status:   k8sresources.SandboxStatusSucceeded,
		ExitCode: 0,
	}
	reconciler := &controller.AgentRunReconciler{
		Client:        c,
		Scheme:        e2eScheme(t),
		SandboxClient: sandbox,
	}
	reconcileRunNTimes(t, reconciler, run.Name, 4)

	var got sprooziv1alpha1.AgentRun
	if err := c.Get(context.Background(), client.ObjectKey{Name: run.Name, Namespace: e2eNamespace}, &got); err != nil {
		t.Fatalf("Get AgentRun: %v", err)
	}
	if got.Status.Phase != sprooziv1alpha1.AgentRunPhaseSucceeded {
		t.Fatalf("expected Succeeded, got %q", got.Status.Phase)
	}
	if len(sandbox.Ensured) == 0 {
		t.Fatal("expected sandbox to be created")
	}
}
