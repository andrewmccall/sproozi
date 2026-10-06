package envtest

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/utils/ptr"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

func TestAgentRuntimeRejectsRootExecution(t *testing.T) {
	t.Parallel()

	scheme := runtime.NewScheme()
	if err := sprooziv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}

	testEnvironment := &envtest.Environment{
		CRDDirectoryPaths: []string{filepath.Join("..", "..", "config", "crd", "bases")},
	}
	configuration, err := testEnvironment.Start()
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		if err := testEnvironment.Stop(); err != nil {
			t.Errorf("Stop() error = %v", err)
		}
	})

	apiClient, err := client.New(configuration, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	valid := validAgentRuntime()
	valid.Name = "codex-mcp-runtime"
	valid.Spec.ClientConfig.Harness = "codex"
	if err := apiClient.Create(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	var stored sprooziv1alpha1.AgentRuntime
	if err := apiClient.Get(context.Background(), client.ObjectKeyFromObject(valid), &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Spec.ClientConfig.Harness != "codex" {
		t.Fatal("client harness selection was pruned")
	}

	rt := validAgentRuntime()
	*rt.Spec.PodTemplate.Spec.SecurityContext.RunAsNonRoot = false
	if err := apiClient.Create(context.Background(), rt); err == nil {
		t.Fatal("Create() error = nil, want API server validation error")
	}
}

func validAgentRuntime() *sprooziv1alpha1.AgentRuntime {
	allowPrivEsc, readOnly, nonRoot := false, true, true
	automount := false
	return &sprooziv1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{Name: testRuntimeName, Namespace: testNamespace},
		Spec: sprooziv1alpha1.AgentRuntimeSpec{
			ClientConfig: sprooziv1alpha1.RuntimeClientConfig{
				TrustBundleConfigMap: sprooziv1alpha1.RuntimeConfigMapKeyReference{
					Name: "sproozi-gateway-ca",
					Key:  "ca.crt",
				},
			},

			EphemeralWorkspace: sprooziv1alpha1.EphemeralWorkspaceSpec{SizeLimit: resource.MustParse("2Gi")},

			PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{AutomountServiceAccountToken: &automount,
				Containers: []corev1.Container{{Name: "agent",
					Image: "ghcr.io/openai/codex@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{
						corev1.ResourceCPU:              resource.MustParse("1"),
						corev1.ResourceMemory:           resource.MustParse("1Gi"),
						corev1.ResourceEphemeralStorage: resource.MustParse("2Gi"),
					}},
					SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &allowPrivEsc,
						ReadOnlyRootFilesystem: &readOnly,
						RunAsNonRoot:           &nonRoot,
						Capabilities:           &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
						SeccompProfile:         &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}}},
				SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true),

					RunAsUser: ptr.
						To(int64(65532)), FSGroup: ptr.To(int64(65532)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.
						SeccompProfileTypeRuntimeDefault,
					}},
			}},
			WorkloadContainers: []string{"agent"}, GatewayEndpoint: "https://sproozi-gateway.sproozi-system.svc:8443",
		},
	}
}

func validTrustBundlePEM(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate trust bundle key: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sproozi-envtest-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create trust bundle certificate: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
}
