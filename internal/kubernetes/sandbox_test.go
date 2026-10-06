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
package kubernetes_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"testing"
	"time"

	"k8s.io/utils/ptr"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/kubernetes"
)

const (
	testClientAuthPlaceholder  = "sproozi-local-placeholder"
	testContractVolumeName     = "contract"
	testGatewayTokenVolumeName = "gateway-token"
	testKubeconfigContextName  = "sproozi"
)

const (
	testTrustBundleName   = "sproozi-sandbox-trust"
	testTrustBundleKey    = "ca-bundle.pem"
	testWorkloadContainer = "agent"
	allCapabilities       = "ALL"
	testGatewayEndpoint   = "https://sproozi-gateway.sproozi-system.svc:8443"
)

func testRuntime() *sprooziv1alpha1.AgentRuntime {
	allowPrivEsc := false
	readOnly := true
	runAsNonRoot := true
	runAsUser := int64(65532)
	return &sprooziv1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{Name: "codex", Namespace: testDefaultNS},
		Spec: sprooziv1alpha1.AgentRuntimeSpec{
			ClientConfig: sprooziv1alpha1.RuntimeClientConfig{
				TrustBundleConfigMap: sprooziv1alpha1.RuntimeConfigMapKeyReference{
					Name: testTrustBundleName,
					Key:  testTrustBundleKey,
				},
			},

			EphemeralWorkspace: sprooziv1alpha1.EphemeralWorkspaceSpec{
				SizeLimit: resource.MustParse("2Gi"),
			},

			PodTemplate: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
				Name: testWorkloadContainer, Image: "ghcr.io/openai/codex@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Resources: corev1.ResourceRequirements{
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("1"),
						corev1.ResourceMemory: resource.MustParse("1Gi"),
					},
				},

				SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &allowPrivEsc, ReadOnlyRootFilesystem: &readOnly, RunAsNonRoot: &runAsNonRoot, RunAsUser: &runAsUser, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{allCapabilities}}, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			}}, AutomountServiceAccountToken: boolPtr(false), SecurityContext: &corev1.
								PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To(int64(65532)),
				FSGroup: ptr.To(int64(65532)), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			}},
			WorkloadContainers: []string{testWorkloadContainer}, GatewayEndpoint: testGatewayEndpoint,
		},
	}
}

func testTrustConfigMap(t *testing.T) *corev1.ConfigMap {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test key: %v", err)
	}
	now := time.Now()
	der, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sproozi-test-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}, &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "sproozi-test-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create test certificate: %v", err)
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: testTrustBundleName, Namespace: kubernetes.AgentsNamespace},
		Immutable:  boolPtr(true),
		Data: map[string]string{
			testTrustBundleKey: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		},
	}
}

func boolPtr(value bool) *bool { return &value }

func testRunForSandbox(uid string) *sprooziv1alpha1.AgentRun {
	return &sprooziv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-run",
			Namespace: testDefaultNS,
			UID:       types.UID(uid),
		},
		Spec: sprooziv1alpha1.AgentRunSpec{
			Task: "Investigate the incident.",
		},
	}
}

// TestBuildPodSpecUsesRuntimeImage verifies the Pod uses the runtime image, not run-controlled data.
func TestBuildPodSpecUsesRuntimeImage(t *testing.T) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000001")
	rt := testRuntime()
	saName := kubernetes.RunName(run.UID)

	pod := kubernetes.BuildPodSpec(run, rt, saName)

	if pod.Spec.Containers[0].Image != rt.Spec.PodTemplate.Spec.Containers[0].Image {
		t.Errorf("image = %q, want %q", pod.Spec.Containers[0].Image, rt.Spec.PodTemplate.Spec.Containers[0].Image)
	}
}

func TestBuildPodSpecPreservesSidecarAndBindsSelectedContainersOnly(t *testing.T) {
	rt := testRuntime()
	falseValue := false
	trueValue := true
	rt.Spec.PodTemplate = corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{
		{Name: testWorkloadContainer, Image: "registry.example/agent@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Resources: corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi")}}, SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &falseValue, ReadOnlyRootFilesystem: &trueValue, RunAsNonRoot: &trueValue, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{allCapabilities}}}},
		{Name: "sidecar", Image: "registry.example/sidecar@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Env: []corev1.EnvVar{{Name: "ADMIN_SETTING", Value: "kept"}}},
	}, Volumes: []corev1.Volume{{Name: "approved", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}}}
	// BuildPodSpec is pure; validation is performed by Ensure at the API/runtime seam.
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000201")
	pod := kubernetes.BuildPodSpec(run, rt, kubernetes.RunName(run.UID))
	if len(pod.Spec.Containers) != 2 || pod.Spec.Containers[1].Env[0].Value != "kept" {
		t.Fatal("approved sidecar was not preserved")
	}
	if len(pod.Spec.Containers[1].VolumeMounts) != 0 {
		t.Fatal("sidecar received run bindings")
	}
	if len(pod.Spec.Containers[0].VolumeMounts) == 0 {
		t.Fatal("workload container did not receive bindings")
	}
}

// TestBuildPodSpecSecurityContext verifies non-root, read-only filesystem, no privilege escalation.
func TestBuildPodSpecSecurityContext(t *testing.T) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000002")
	rt := testRuntime()
	pod := kubernetes.BuildPodSpec(run, rt, kubernetes.RunName(run.UID))

	sc := pod.Spec.Containers[0].SecurityContext
	if sc == nil {
		t.Fatal("SecurityContext is nil")
	}
	if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
		t.Error("AllowPrivilegeEscalation must be false")
	}
	if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
		t.Error("ReadOnlyRootFilesystem must be true")
	}
	if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
		t.Error("RunAsNonRoot must be true")
	}
	if sc.RunAsUser == nil || *sc.RunAsUser != 65532 {
		t.Errorf("RunAsUser = %v, want 65532", sc.RunAsUser)
	}
	if pod.Spec.SecurityContext == nil || pod.Spec.SecurityContext.FSGroup == nil ||
		*pod.Spec.SecurityContext.FSGroup != 65532 {
		t.Error("FSGroup must allow the non-root agent to write its emptyDir volumes")
	}
}

// TestBuildPodSpecRestartNever verifies the Pod never restarts.
func TestBuildPodSpecRestartNever(t *testing.T) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000003")
	rt := testRuntime()
	pod := kubernetes.BuildPodSpec(run, rt, kubernetes.RunName(run.UID))

	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %q, want Never", pod.Spec.RestartPolicy)
	}
}

// TestBuildPodSpecServiceAccount verifies the Pod uses the provided SA.
func TestBuildPodSpecServiceAccount(t *testing.T) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000004")
	rt := testRuntime()
	saName := kubernetes.RunName(run.UID)
	pod := kubernetes.BuildPodSpec(run, rt, saName)

	if pod.Spec.ServiceAccountName != saName {
		t.Errorf("ServiceAccountName = %q, want %q", pod.Spec.ServiceAccountName, saName)
	}
}

// TestBuildPodSpecGatewayEnvVars verifies the shared gateway binding.
func TestBuildPodSpecGatewayEnvVars(t *testing.T) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000005")
	rt := testRuntime()
	pod := kubernetes.BuildPodSpec(run, rt, kubernetes.RunName(run.UID))

	envMap := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		envMap[e.Name] = e.Value
	}

	if envMap["SPROOZI_GATEWAY"] != testGatewayEndpoint {
		t.Errorf("SPROOZI_GATEWAY = %q, want shared gateway", envMap["SPROOZI_GATEWAY"])
	}
	if envMap["OPENAI_API_KEY"] != testClientAuthPlaceholder {
		t.Errorf("OPENAI_API_KEY = %q, want local gateway placeholder", envMap["OPENAI_API_KEY"])
	}
	if envMap["OPENAI_BASE_URL"] != "https://api.openai.com/v1" {
		t.Errorf("OPENAI_BASE_URL = %q, want %q", envMap["OPENAI_BASE_URL"], "https://api.openai.com/v1")
	}
	if envMap["KUBECONFIG"] != "/etc/sproozi/kubeconfig" {
		t.Errorf("KUBECONFIG = %q, want /etc/sproozi/kubeconfig", envMap["KUBECONFIG"])
	}
	if _, present := envMap["SPROOZI_EGRESS_GATEWAY"]; present {
		t.Error("egress gateway must not be configured without network.egress")
	}
	if _, present := envMap["CODEX_TASK"]; present {
		t.Error("untrusted task must not be injected as an environment variable")
	}
}

func TestBuildPodSpecHardensIdentityAndTokens(t *testing.T) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000099")
	pod := kubernetes.BuildPodSpec(run, testRuntime(), kubernetes.RunName(run.UID))

	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("default ServiceAccount token must be disabled")
	}
	if pod.Spec.HostNetwork || pod.Spec.HostPID || pod.Spec.HostIPC {
		t.Fatal("host namespaces must be disabled")
	}
	security := pod.Spec.Containers[0].SecurityContext
	if security.SeccompProfile == nil || security.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("container must require RuntimeDefault seccomp")
	}
	if security.Capabilities == nil || len(security.Capabilities.Drop) != 1 || security.Capabilities.Drop[0] != allCapabilities {
		t.Fatal("container must drop all Linux capabilities")
	}

	audiences := map[string]string{}
	for _, volume := range pod.Spec.Volumes {
		if volume.Projected == nil || len(volume.Projected.Sources) != 1 || volume.Projected.Sources[0].ServiceAccountToken == nil {
			continue
		}
		audiences[volume.Name] = volume.Projected.Sources[0].ServiceAccountToken.Audience
	}
	if audiences[testGatewayTokenVolumeName] != "sproozi-gateway" {
		t.Errorf("gateway-token audience = %q, want sproozi-gateway", audiences[testGatewayTokenVolumeName])
	}
	if _, present := audiences["kubernetes-token"]; present {
		t.Fatal("sandbox must not receive a native Kubernetes token")
	}
}

func TestBuildPodSpecMountsAdministratorTrustAndGeneratedKubeconfigReadOnly(t *testing.T) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000098")
	rt := testRuntime()
	pod := kubernetes.BuildPodSpec(run, rt, kubernetes.RunName(run.UID))

	var trustVolume *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "client-trust" {
			trustVolume = &pod.Spec.Volumes[i]
		}
	}
	if trustVolume == nil || trustVolume.Projected == nil {
		t.Fatal("Pod must project administrator-owned trust material")
	}
	wantSources := map[string]string{testTrustBundleName: testTrustBundleKey}
	for _, source := range trustVolume.Projected.Sources {
		if source.ConfigMap == nil || len(source.ConfigMap.Items) != 1 {
			t.Fatalf("unexpected trust projection: %#v", source)
		}
		if wantSources[source.ConfigMap.Name] != source.ConfigMap.Items[0].Key {
			t.Errorf("unexpected trust source %q/%q", source.ConfigMap.Name, source.ConfigMap.Items[0].Key)
		}
		delete(wantSources, source.ConfigMap.Name)
	}
	if len(wantSources) != 0 {
		t.Fatalf("missing trust sources: %v", wantSources)
	}

	mounts := map[string]corev1.VolumeMount{}
	for _, mount := range pod.Spec.Containers[0].VolumeMounts {
		mounts[mount.MountPath] = mount
	}
	if mount := mounts["/etc/sproozi/trust"]; mount.Name != "client-trust" || !mount.ReadOnly {
		t.Fatalf("trust mount = %#v, want read-only client-trust", mount)
	}
	if mount := mounts["/etc/sproozi/kubeconfig"]; mount.Name != testContractVolumeName || mount.SubPath != "kubeconfig" || !mount.ReadOnly {
		t.Fatalf("kubeconfig mount = %#v, want read-only generated contract key", mount)
	}
}

func TestBuildPodSpecAddsEgressTokenOnlyWhenRequested(t *testing.T) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000100")
	run.Spec.Capabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityNetworkEgress}
	pod := kubernetes.BuildPodSpec(run, testRuntime(), kubernetes.RunName(run.UID))

	foundVolume, foundMount, foundEnv := false, false, false
	for _, volume := range pod.Spec.Volumes {
		foundVolume = foundVolume || volume.Name == testGatewayTokenVolumeName
	}
	for _, mount := range pod.Spec.Containers[0].VolumeMounts {
		foundMount = foundMount || mount.Name == testGatewayTokenVolumeName
	}
	for _, env := range pod.Spec.Containers[0].Env {
		foundEnv = foundEnv || (env.Name == "SPROOZI_GATEWAY" && env.Value == testGatewayEndpoint)
	}
	if !foundVolume || !foundMount || !foundEnv {
		t.Fatal("network.egress must add the egress gateway and projected audience token")
	}
}

func TestBuildPodSpecGatewayTokenCoversMaximumRunDuration(t *testing.T) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000101")
	pod := kubernetes.BuildPodSpec(run, testRuntime(), kubernetes.RunName(run.UID))

	var expirationSeconds int64
	for _, volume := range pod.Spec.Volumes {
		if volume.Name != testGatewayTokenVolumeName || volume.Projected == nil {
			continue
		}
		for _, source := range volume.Projected.Sources {
			if source.ServiceAccountToken != nil && source.ServiceAccountToken.ExpirationSeconds != nil {
				expirationSeconds = *source.ServiceAccountToken.ExpirationSeconds
			}
		}
	}
	if expirationSeconds < 2*60*60 {
		t.Fatalf("gateway token lifetime = %d seconds, want at least 7200 seconds to cover the one-hour run bound with headroom", expirationSeconds)
	}
}

// TestBuildPodSpecNamespace verifies the Pod is in AgentsNamespace.
func TestBuildPodSpecNamespace(t *testing.T) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000006")
	rt := testRuntime()
	pod := kubernetes.BuildPodSpec(run, rt, kubernetes.RunName(run.UID))

	if pod.Namespace != kubernetes.AgentsNamespace {
		t.Errorf("Namespace = %q, want %q", pod.Namespace, kubernetes.AgentsNamespace)
	}
}

// TestBuildPodSpecLabels verifies RunLabel, RunNamespaceLabel, and ManagedByLabel are set
// so that the Pod watcher can map sandbox Pod events back to the owning AgentRun.
func TestBuildPodSpecLabels(t *testing.T) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000010")
	rt := testRuntime()
	pod := kubernetes.BuildPodSpec(run, rt, kubernetes.RunName(run.UID))

	if got := pod.Labels[kubernetes.RunLabel]; got != run.Name {
		t.Errorf("RunLabel = %q, want %q", got, run.Name)
	}
	if got := pod.Labels[kubernetes.RunNamespaceLabel]; got != run.Namespace {
		t.Errorf("RunNamespaceLabel = %q, want %q", got, run.Namespace)
	}
	if got := pod.Labels[kubernetes.ManagedByLabel]; got != kubernetes.ManagedByValue {
		t.Errorf("ManagedByLabel = %q, want %q", got, kubernetes.ManagedByValue)
	}
}

// TestBuildPodSpecWorkspaceVolume verifies an emptyDir workspace volume with size limit.
func TestBuildPodSpecWorkspaceVolume(t *testing.T) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000007")
	rt := testRuntime()
	pod := kubernetes.BuildPodSpec(run, rt, kubernetes.RunName(run.UID))

	var workspaceVol *corev1.Volume
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == "workspace" {
			workspaceVol = &pod.Spec.Volumes[i]
		}
	}
	if workspaceVol == nil {
		t.Fatal("workspace volume not found")
	}
	if workspaceVol.EmptyDir == nil {
		t.Fatal("workspace volume is not an emptyDir")
	}
	if workspaceVol.EmptyDir.SizeLimit == nil {
		t.Fatal("workspace emptyDir has no size limit")
	}
}

// TestEnsureSandboxCreates verifies Ensure creates a Pod in sproozi-agents.
func TestEnsureSandboxCreates(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t)).Build()
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000008")
	rt := testRuntime()

	sc := kubernetes.NewPodSandboxClient(c) // nil logsFn; not called on create
	name, err := sc.Ensure(context.Background(), run, rt)
	if err != nil {
		t.Fatalf("Ensure error: %v", err)
	}
	if name != kubernetes.RunName(run.UID) {
		t.Errorf("name = %q, want %q", name, kubernetes.RunName(run.UID))
	}

	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: kubernetes.AgentsNamespace, Name: name}, &pod); err != nil {
		t.Fatalf("Pod not found: %v", err)
	}
}

// TestEnsureSandboxIdempotent verifies calling Ensure twice does not error.
func TestEnsureSandboxIdempotent(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t)).Build()
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000009")
	rt := testRuntime()
	sc := kubernetes.NewPodSandboxClient(c)

	if _, err := sc.Ensure(context.Background(), run, rt); err != nil {
		t.Fatalf("first Ensure error: %v", err)
	}
	if _, err := sc.Ensure(context.Background(), run, rt); err != nil {
		t.Fatalf("second Ensure error: %v", err)
	}
}

func TestEnsureSandboxRejectsPreexistingIdentityCollision(t *testing.T) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000098")
	rt := testRuntime()
	wanted := kubernetes.BuildPodSpec(run, rt, kubernetes.RunName(run.UID))
	collision := wanted.DeepCopy()
	collision.Spec.Containers[0].Image = "attacker.invalid/runner:latest"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t), collision).Build()

	if _, err := kubernetes.NewPodSandboxClient(c).Ensure(context.Background(), run, rt); err == nil {
		t.Fatal("Ensure accepted a preexisting Pod with a different image")
	}
}

func TestEnsureSandboxRejectsMismatchedRunLabels(t *testing.T) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000099")
	rt := testRuntime()
	wanted := kubernetes.BuildPodSpec(run, rt, kubernetes.RunName(run.UID))
	collision := wanted.DeepCopy()
	collision.Labels[kubernetes.RunUIDLabel] = "different-run-uid"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t), collision).Build()

	if _, err := kubernetes.NewPodSandboxClient(c).Ensure(context.Background(), run, rt); err == nil {
		t.Fatal("Ensure accepted a preexisting Pod with a different run UID")
	}
}

func TestEnsureSandboxRejectsMismatchedAdditionalContainers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*corev1.Pod)
	}{
		{
			name: "init container",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.InitContainers = []corev1.Container{{Name: "unexpected", Image: "attacker.invalid/init:latest"}}
			},
		},
		{
			name: "ephemeral container",
			mutate: func(pod *corev1.Pod) {
				pod.Spec.EphemeralContainers = []corev1.EphemeralContainer{{EphemeralContainerCommon: corev1.EphemeralContainerCommon{Name: "unexpected", Image: "attacker.invalid/debug:latest"}}}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := testRunForSandbox("cccccccc-0000-0000-0000-000000000102")
			rt := testRuntime()
			wanted := kubernetes.BuildPodSpec(run, rt, kubernetes.RunName(run.UID))
			collision := wanted.DeepCopy()
			tc.mutate(collision)
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t), collision).Build()

			if _, err := kubernetes.NewPodSandboxClient(c).Ensure(context.Background(), run, rt); err == nil {
				t.Fatalf("Ensure accepted a preexisting Pod with an unexpected %s", tc.name)
			}
		})
	}
}

func TestEnsureSandboxFailsClosedWithoutImmutableCertificateOnlyTrustBundle(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configMap *corev1.ConfigMap
	}{
		{name: "missing"},
		{name: "mutable", configMap: func() *corev1.ConfigMap {
			cm := testTrustConfigMap(t)
			cm.Immutable = nil
			return cm
		}()},
		{name: "private-key", configMap: &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: testTrustBundleName, Namespace: kubernetes.AgentsNamespace},
			Immutable:  boolPtr(true),
			Data:       map[string]string{testTrustBundleKey: "-----BEGIN PRIVATE KEY-----\nforbidden\n-----END PRIVATE KEY-----\n"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(testScheme(t))
			if tc.configMap != nil {
				builder = builder.WithObjects(tc.configMap)
			}
			c := builder.Build()
			run := testRunForSandbox("cccccccc-0000-0000-0000-000000000097")
			_, err := kubernetes.NewPodSandboxClient(c).Ensure(context.Background(), run, testRuntime())
			if err == nil {
				t.Fatal("Ensure must reject unavailable, mutable, or non-certificate trust material")
			}
			var pod corev1.Pod
			if getErr := c.Get(context.Background(), client.ObjectKey{Namespace: kubernetes.AgentsNamespace, Name: kubernetes.RunName(run.UID)}, &pod); getErr == nil {
				t.Fatal("Ensure created a Pod after trust validation failed")
			}
		})
	}
}

// TestDeleteSandboxNotFoundIsOK verifies Delete on a missing Pod does not error.
func TestDeleteSandboxNotFoundIsOK(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	sc := kubernetes.NewPodSandboxClient(c)
	if err := sc.Delete(context.Background(), "sproozi-nonexistent"); err != nil {
		t.Errorf("Delete on missing Pod should not error: %v", err)
	}
}

// TestDeleteSandboxDeletes verifies Delete removes the Pod.
func TestDeleteSandboxDeletes(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t)).Build()
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000010")
	rt := testRuntime()
	sc := kubernetes.NewPodSandboxClient(c)

	name, _ := sc.Ensure(context.Background(), run, rt)
	if err := sc.Delete(context.Background(), name); err != nil {
		t.Fatalf("Delete error: %v", err)
	}

	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: kubernetes.AgentsNamespace, Name: name}, &pod); err == nil {
		t.Error("Pod still exists after Delete")
	}
}

// TestGetStatusRunning verifies GetStatus returns Running for a running Pod.
func TestGetStatusRunning(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&corev1.Pod{}).WithObjects(testTrustConfigMap(t)).Build()
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000011")
	rt := testRuntime()
	sc := kubernetes.NewPodSandboxClient(c)

	name, _ := sc.Ensure(context.Background(), run, rt)

	// Patch Pod phase to Running via status subresource.
	var pod corev1.Pod
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: kubernetes.AgentsNamespace, Name: name}, &pod)
	pod.Status.Phase = corev1.PodRunning
	_ = c.Status().Update(context.Background(), &pod)

	status, err := sc.GetStatus(context.Background(), name)
	if err != nil {
		t.Fatalf("GetStatus error: %v", err)
	}
	if status != kubernetes.SandboxStatusRunning {
		t.Errorf("status = %q, want Running", status)
	}
}

// TestGetStatusUnknownWhenNotFound verifies GetStatus returns Unknown for missing Pod.
func TestGetStatusUnknownWhenNotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	sc := kubernetes.NewPodSandboxClient(c)

	status, err := sc.GetStatus(context.Background(), "sproozi-doesnotexist")
	if err != nil {
		t.Fatalf("GetStatus error: %v", err)
	}
	if status != kubernetes.SandboxStatusUnknown {
		t.Errorf("status = %q, want Unknown", status)
	}
}

// TestGetExitCode verifies GetExitCode reads the exit code from the agent container status.
func TestGetExitCode(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&corev1.Pod{}).WithObjects(testTrustConfigMap(t)).Build()
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000020")
	rt := testRuntime()
	sc := kubernetes.NewPodSandboxClient(c)

	name, _ := sc.Ensure(context.Background(), run, rt)

	// Set the container status with a terminated state.
	var pod corev1.Pod
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: kubernetes.AgentsNamespace, Name: name}, &pod)
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: testWorkloadContainer,
		State: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "strict result"},
		},
	}}
	_ = c.Status().Update(context.Background(), &pod)

	exitCode, err := sc.GetExitCode(context.Background(), name)
	if err != nil {
		t.Fatalf("GetExitCode error: %v", err)
	}
	if exitCode != 1 {
		t.Errorf("exitCode = %d, want 1", exitCode)
	}
}
