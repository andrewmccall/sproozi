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
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/utils/ptr"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
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
		ObjectMeta: metav1.ObjectMeta{Name: testCodexHarness, Namespace: testDefaultNS},
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

// TestBuildPodSpecFixedFields verifies the fields every sandbox Pod takes from
// the runtime and Sproozi rather than from run-controlled data. The labels let
// the Pod watcher map sandbox Pod events back to the owning AgentRun.
func TestBuildPodSpecFixedFields(t *testing.T) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000001")
	rt := testRuntime()
	saName := kubernetes.RunName(run.UID)
	pod := kubernetes.BuildPodSpec(run, rt, saName)

	boundedWorkspace := false
	for _, volume := range pod.Spec.Volumes {
		if volume.Name == "workspace" {
			boundedWorkspace = volume.EmptyDir != nil && volume.EmptyDir.SizeLimit != nil
		}
	}
	for _, field := range []struct {
		name      string
		got, want any
	}{
		{"image", pod.Spec.Containers[0].Image, rt.Spec.PodTemplate.Spec.Containers[0].Image},
		{"RestartPolicy", pod.Spec.RestartPolicy, corev1.RestartPolicyNever},
		{"ServiceAccountName", pod.Spec.ServiceAccountName, saName},
		{"Namespace", pod.Namespace, kubernetes.AgentsNamespace},
		{"RunLabel", pod.Labels[kubernetes.RunLabel], run.Name},
		{"RunNamespaceLabel", pod.Labels[kubernetes.RunNamespaceLabel], run.Namespace},
		{"ManagedByLabel", pod.Labels[kubernetes.ManagedByLabel], kubernetes.ManagedByValue},
		{"size-limited emptyDir workspace", boundedWorkspace, true},
	} {
		if field.got != field.want {
			t.Errorf("%s = %v, want %v", field.name, field.got, field.want)
		}
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

// TestEnsureSandboxCreates verifies Ensure creates a Pod in sproozi-agents.
func TestEnsureSandboxCreates(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t)).Build()
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000008")
	rt := testRuntime()

	sc := newSandboxClient(c) // nil logsFn; not called on create
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
	sc := newSandboxClient(c)

	if _, err := sc.Ensure(context.Background(), run, rt); err != nil {
		t.Fatalf("first Ensure error: %v", err)
	}
	if _, err := sc.Ensure(context.Background(), run, rt); err != nil {
		t.Fatalf("second Ensure error: %v", err)
	}
}

// A Quantity caches its string form lazily, so equal workspace sizes can
// differ under reflect.DeepEqual. Release runtimes use 256Mi, which the parser
// does not cache, so a retry rejected the controller's own Pod.
func TestEnsureSandboxAcceptsEqualSizesWithDifferentCachedForm(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t)).Build()
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000010")
	rt := testRuntime()
	rt.Spec.EphemeralWorkspace.SizeLimit = resource.MustParse("256Mi")
	sc := newSandboxClient(c)

	if _, err := sc.Ensure(context.Background(), run, rt); err != nil {
		t.Fatalf("first Ensure error: %v", err)
	}
	_ = rt.Spec.EphemeralWorkspace.SizeLimit.String()
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

	if _, err := newSandboxClient(c).Ensure(context.Background(), run, rt); err == nil {
		t.Fatal("Ensure accepted a preexisting Pod with a different image")
	}
}

func TestEnsureSandboxComparesRuntimeClassByValue(t *testing.T) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000103")
	rt := testRuntime()
	rt.Spec.RuntimeClassName = "gvisor"
	existing := pinnedPod(t, kubernetes.BuildPodSpec(run, rt, run.Status.Identity.ServiceAccountName), rt)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t), existing).Build()
	sc := newSandboxClient(c)

	if _, err := sc.Ensure(context.Background(), run, rt); err != nil {
		t.Fatalf("Ensure rejected a matching Pod with a runtime class: %v", err)
	}
	rt.Spec.RuntimeClassName = "runc"
	if _, err := sc.Ensure(context.Background(), run, rt); err == nil {
		t.Fatal("Ensure accepted a preexisting Pod with a different runtime class")
	}
}

// defaultedRuntime leaves unset the fields the API server defaults on a Pod.
func defaultedRuntime() *sprooziv1alpha1.AgentRuntime {
	rt := testRuntime()
	spec := &rt.Spec.PodTemplate.Spec
	spec.Volumes = []corev1.Volume{
		{Name: "cfg", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
			LocalObjectReference: corev1.LocalObjectReference{Name: "cfg"}}}},
		{Name: "info", VolumeSource: corev1.VolumeSource{DownwardAPI: &corev1.DownwardAPIVolumeSource{
			Items: []corev1.DownwardAPIVolumeFile{{Path: "labels", FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.labels"}}}}}},
	}
	agent := &spec.Containers[0]
	agent.Ports = []corev1.ContainerPort{{ContainerPort: 8080}}
	agent.Env = []corev1.EnvVar{{Name: "POD", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}}}
	agent.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}}
	agent.LivenessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(8080)}}}
	agent.Lifecycle = &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{HTTPGet: &corev1.HTTPGetAction{Port: intstr.FromInt32(8080)}}}
	return rt
}

// applyServerDefaults sets what the API server stored for defaultedRuntime,
// as observed against a real kube-apiserver.
func applyServerDefaults(pod *corev1.Pod) {
	mode := int32(0o644)
	for i := range pod.Spec.Volumes {
		volume := &pod.Spec.Volumes[i].VolumeSource
		if volume.ConfigMap != nil && volume.ConfigMap.DefaultMode == nil {
			volume.ConfigMap.DefaultMode = &mode
		}
		if volume.DownwardAPI != nil {
			volume.DownwardAPI.DefaultMode = &mode
			volume.DownwardAPI.Items[0].FieldRef.APIVersion = "v1"
		}
	}
	agent := &pod.Spec.Containers[0]
	agent.Ports[0].Protocol = corev1.ProtocolTCP
	agent.Env[0].ValueFrom.FieldRef.APIVersion = "v1"
	for _, probe := range []*corev1.Probe{agent.ReadinessProbe, agent.LivenessProbe} {
		probe.TimeoutSeconds, probe.PeriodSeconds, probe.SuccessThreshold, probe.FailureThreshold = 1, 10, 1, 3
	}
	agent.LivenessProbe.HTTPGet.Path, agent.LivenessProbe.HTTPGet.Scheme = "/", corev1.URISchemeHTTP
	agent.Lifecycle.PreStop.HTTPGet.Path, agent.Lifecycle.PreStop.HTTPGet.Scheme = "/", corev1.URISchemeHTTP
}

// A Pod the API server stored with defaults filled in is the same sandbox; a
// defaulted Pod that also differs in a set field is still rejected.
func TestEnsureSandboxAcceptsServerDefaultedPod(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*corev1.Pod)
		accept bool
	}{
		{name: "defaults only", mutate: func(*corev1.Pod) {}, accept: true},
		{name: "privileged container", mutate: func(pod *corev1.Pod) {
			pod.Spec.Containers[0].SecurityContext.Privileged = ptr.To(true)
		}},
		{name: "writable ConfigMap volume", mutate: func(pod *corev1.Pod) {
			pod.Spec.Volumes[0].ConfigMap.DefaultMode = ptr.To(int32(0o777))
		}},
		{name: "longer probe period", mutate: func(pod *corev1.Pod) {
			pod.Spec.Containers[0].ReadinessProbe.PeriodSeconds = 30
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			run := testRunForSandbox("cccccccc-0000-0000-0000-000000000104")
			rt := defaultedRuntime()
			existing := pinnedPod(t, kubernetes.BuildPodSpec(run, rt, run.Status.Identity.ServiceAccountName), rt)
			applyServerDefaults(existing)
			tc.mutate(existing)
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t), existing).Build()

			_, err := newSandboxClient(c).Ensure(context.Background(), run, rt)
			if tc.accept && err != nil {
				t.Fatalf("Ensure rejected a server-defaulted sandbox: %v", err)
			}
			if !tc.accept && err == nil {
				t.Fatal("Ensure accepted a defaulted sandbox with a changed setting")
			}
		})
	}
}

func TestEnsureSandboxRejectsMismatchedRunLabels(t *testing.T) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000099")
	rt := testRuntime()
	wanted := kubernetes.BuildPodSpec(run, rt, kubernetes.RunName(run.UID))
	collision := wanted.DeepCopy()
	collision.Labels[kubernetes.RunUIDLabel] = "different-run-uid"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t), collision).Build()

	if _, err := newSandboxClient(c).Ensure(context.Background(), run, rt); err == nil {
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

			if _, err := newSandboxClient(c).Ensure(context.Background(), run, rt); err == nil {
				t.Fatalf("Ensure accepted a preexisting Pod with an unexpected %s", tc.name)
			}
		})
	}
}

func TestValidateRuntimeRejectsUnsafeTemplate(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t)).Build()
	rt := testRuntime()
	rt.Spec.PodTemplate.Spec.HostNetwork = true
	if err := newSandboxClient(c).ValidateRuntime(context.Background(), rt); !errors.Is(err, kubernetes.ErrInvalidRuntime) {
		t.Fatalf("ValidateRuntime error = %v, want ErrInvalidRuntime", err)
	}
}

func TestValidateRuntimeFailsClosedWithoutImmutableCertificateOnlyTrustBundle(t *testing.T) {
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
			err := newSandboxClient(c).ValidateRuntime(context.Background(), testRuntime())
			if !errors.Is(err, kubernetes.ErrInvalidRuntime) {
				t.Fatalf("ValidateRuntime error = %v, want ErrInvalidRuntime for unavailable, mutable, or non-certificate trust material", err)
			}
		})
	}
}

// TestDeleteSandboxNotFoundIsOK verifies DeleteForRun on a missing Pod does not error.
func TestDeleteSandboxNotFoundIsOK(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	sc := newSandboxClient(c)
	if err := sc.DeleteForRun(context.Background(), "sproozi-nonexistent", "missing-run"); err != nil {
		t.Errorf("DeleteForRun on missing Pod should not error: %v", err)
	}
}

// TestDeleteSandboxDeletes verifies DeleteForRun removes the run's Pod.
func TestDeleteSandboxDeletes(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t)).Build()
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000010")
	rt := testRuntime()
	sc := newSandboxClient(c)

	name, _ := sc.Ensure(context.Background(), run, rt)
	if err := sc.DeleteForRun(context.Background(), name, string(run.UID)); err != nil {
		t.Fatalf("DeleteForRun error: %v", err)
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
	sc := newSandboxClient(c)

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

// TestGetStatusAbsentWhenNotFound verifies GetStatus returns Absent for missing Pod.
func TestGetStatusAbsentWhenNotFound(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	sc := newSandboxClient(c)

	status, err := sc.GetStatus(context.Background(), "sproozi-doesnotexist")
	if err != nil {
		t.Fatalf("GetStatus error: %v", err)
	}
	if status != kubernetes.SandboxStatusAbsent {
		t.Errorf("status = %q, want Absent", status)
	}
}

// TestGetCompletion reads actual exit independently of optional result availability.
func TestGetCompletion(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&corev1.Pod{}).WithObjects(testTrustConfigMap(t)).Build()
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000020")
	rt := testRuntime()
	sc := newSandboxClient(c)

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

	completion, err := sc.GetCompletion(context.Background(), name, string(run.UID))
	if err != nil {
		t.Fatalf("GetCompletion error: %v", err)
	}
	if completion.ExitCode != 1 || completion.Result != nil {
		t.Errorf("completion = %#v, want exit 1 with unavailable result", completion)
	}

	valid := `{"version":"sproozi.run-result/v1","runUID":"cccccccc-0000-0000-0000-000000000020","result":{"text":"Answer café","truncated":true}}`
	for _, tc := range []struct {
		name, message string
		exitCode      int32
		want          *sprooziv1alpha1.AgentRunResult
	}{
		{"success", valid, 0, &sprooziv1alpha1.AgentRunResult{Text: "Answer café", Truncated: true}},
		{"failure with answer", valid, 7, &sprooziv1alpha1.AgentRunResult{Text: "Answer café", Truncated: true}},
		{"missing answer", "", 0, nil},
		{"malformed answer", "{", 0, nil},
		{"foreign answer", strings.Replace(valid, string(run.UID), "other-run", 1), 0, nil},
		{"available empty", strings.Replace(valid, "Answer café", "", 1), 0, &sprooziv1alpha1.AgentRunResult{Truncated: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod.Status.ContainerStatuses[0].State.Terminated = &corev1.ContainerStateTerminated{ExitCode: tc.exitCode, Message: tc.message}
			if err := c.Status().Update(context.Background(), &pod); err != nil {
				t.Fatal(err)
			}
			got, err := sc.GetCompletion(context.Background(), name, string(run.UID))
			if err != nil || got.ExitCode != tc.exitCode || !reflect.DeepEqual(got.Result, tc.want) {
				t.Fatalf("completion = %#v, %v; want exit %d, result %#v", got, err, tc.exitCode, tc.want)
			}
		})
	}
	// The same well-framed answer cannot be sourced from fallback container logs.
	pod.Spec.Containers[0].TerminationMessagePolicy = corev1.TerminationMessageFallbackToLogsOnError
	if err := c.Update(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	got, err := sc.GetCompletion(context.Background(), name, string(run.UID))
	if err != nil || got.ExitCode != 0 || got.Result != nil {
		t.Fatalf("fallback-log completion = %#v, %v; want exit 0 with unavailable result", got, err)
	}
	// Changing only Pod ownership cannot feed a foreign answer or exit into the run.
	pod.Labels[kubernetes.RunUIDLabel] = "replacement-run"
	if err := c.Update(context.Background(), &pod); err != nil {
		t.Fatal(err)
	}
	if _, err := sc.GetCompletion(context.Background(), name, string(run.UID)); err == nil {
		t.Fatal("accepted foreign Pod completion")
	}
}

func TestGetStatusUnknownForExistingUncertainPod(t *testing.T) {
	for _, phase := range []corev1.PodPhase{"", corev1.PodUnknown} {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "uncertain", Namespace: kubernetes.AgentsNamespace}, Status: corev1.PodStatus{Phase: phase}}
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pod).Build()
		status, err := newSandboxClient(c).GetStatus(context.Background(), pod.Name)
		if err != nil || status != kubernetes.SandboxStatusUnknown {
			t.Fatalf("existing phase %q = (%s,%v)", phase, status, err)
		}
	}
}

func TestGetStatusConfirmsCachedAbsenceWithLiveRead(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "just-created", Namespace: kubernetes.AgentsNamespace}, Status: corev1.PodStatus{Phase: corev1.PodPending}}
	cache := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	live := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pod).Build()
	// A cache that has not yet observed the Pod must not end the run.
	status, err := kubernetes.NewPodSandboxClient(cache, live).GetStatus(context.Background(), pod.Name)
	if err != nil || status != kubernetes.SandboxStatusPending {
		t.Fatalf("lagging cache = (%s,%v); want Pending", status, err)
	}
	status, err = kubernetes.NewPodSandboxClient(cache, cache).GetStatus(context.Background(), pod.Name)
	if err != nil || status != kubernetes.SandboxStatusAbsent {
		t.Fatalf("confirmed absence = (%s,%v); want Absent", status, err)
	}
}

func TestIsQuotaExceeded(t *testing.T) {
	t.Parallel()

	pods := corev1.Resource("pods")
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil},
		{name: "quota exceeded", err: apierrors.NewForbidden(pods, "sandbox", errors.New("exceeded quota: agents, requested: pods=1, used: pods=2, limited: pods=2")), want: true},
		{name: "wrapped quota exceeded", err: fmt.Errorf("create sandbox: %w", apierrors.NewForbidden(pods, "sandbox", errors.New("exceeded quota: agents"))), want: true},
		{name: "other forbidden", err: apierrors.NewForbidden(pods, "sandbox", errors.New("violates PodSecurity"))},
		{name: "not forbidden", err: apierrors.NewInternalError(errors.New("exceeded quota"))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := kubernetes.IsQuotaExceeded(tt.err); got != tt.want {
				t.Fatalf("IsQuotaExceeded(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// testGatewayAddress stands in for the gateway Service's cluster IP.
const testGatewayAddress = "10.96.0.42"

func staticGateway(context.Context, string) (string, error) { return testGatewayAddress, nil }

func newSandboxClient(c client.Client) *kubernetes.PodSandboxClient {
	return kubernetes.NewPodSandboxClient(c, c).WithGatewayResolver(staticGateway)
}

// pinnedPod applies what Ensure adds to a built Pod, for tests that store a
// sandbox before Ensure compares against it.
func pinnedPod(t *testing.T, pod *corev1.Pod, rt *sprooziv1alpha1.AgentRuntime) *corev1.Pod {
	t.Helper()
	if err := kubernetes.PinGateway(context.Background(), pod, rt.Spec.GatewayEndpoint, staticGateway); err != nil {
		t.Fatal(err)
	}
	return pod
}

// A sandbox gets no resolver: it reaches the gateway by a pinned address, so
// DNS cannot carry data past the gateway.
func TestEnsureSandboxPinsGatewayAndRemovesDNS(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t)).Build()
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000120")
	rt := testRuntime()
	rt.Spec.PodTemplate.Spec.DNSPolicy = corev1.DNSClusterFirst
	rt.Spec.PodTemplate.Spec.HostAliases = []corev1.HostAlias{{IP: "203.0.113.9", Hostnames: []string{"api.openai.com"}}}

	name, err := newSandboxClient(c).Ensure(context.Background(), run, rt)
	if err != nil {
		t.Fatal(err)
	}
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: kubernetes.AgentsNamespace, Name: name}, &pod); err != nil {
		t.Fatal(err)
	}
	if pod.Spec.DNSPolicy != corev1.DNSNone || pod.Spec.DNSConfig == nil ||
		!slices.Equal(pod.Spec.DNSConfig.Nameservers, []string{"127.0.0.1"}) || len(pod.Spec.DNSConfig.Searches) != 0 {
		t.Fatalf("DNS = %q %+v, want None with only a loopback nameserver", pod.Spec.DNSPolicy, pod.Spec.DNSConfig)
	}
	want := []corev1.HostAlias{{IP: testGatewayAddress, Hostnames: []string{"sproozi-gateway.sproozi-system.svc"}}}
	if !reflect.DeepEqual(pod.Spec.HostAliases, want) {
		t.Fatalf("hostAliases = %+v, want only the gateway %+v", pod.Spec.HostAliases, want)
	}
}

func TestEnsureSandboxGatewayAddressEndpointNeedsNoAlias(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t)).Build()
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000121")
	rt := testRuntime()
	rt.Spec.GatewayEndpoint = "https://10.96.0.7:8443"
	failing := func(context.Context, string) (string, error) { return "", errors.New("must not resolve an IP literal") }

	name, err := kubernetes.NewPodSandboxClient(c, c).WithGatewayResolver(failing).Ensure(context.Background(), run, rt)
	if err != nil {
		t.Fatal(err)
	}
	var pod corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: kubernetes.AgentsNamespace, Name: name}, &pod); err != nil {
		t.Fatal(err)
	}
	if pod.Spec.DNSPolicy != corev1.DNSNone || len(pod.Spec.HostAliases) != 0 {
		t.Fatalf("DNS = %q aliases = %+v", pod.Spec.DNSPolicy, pod.Spec.HostAliases)
	}
}

func TestEnsureSandboxFailsWhenGatewayDoesNotResolve(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t)).Build()
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000122")
	failing := func(context.Context, string) (string, error) { return "", errors.New("no such host") }
	if _, err := kubernetes.NewPodSandboxClient(c, c).WithGatewayResolver(failing).Ensure(context.Background(), run, testRuntime()); err == nil {
		t.Fatal("Ensure created a sandbox without a gateway address")
	}
	var pods corev1.PodList
	if err := c.List(context.Background(), &pods); err != nil || len(pods.Items) != 0 {
		t.Fatalf("pods = %d err = %v, want none", len(pods.Items), err)
	}
}

// A same-named Pod that keeps cluster DNS is not the hardened sandbox.
func TestEnsureSandboxRejectsExistingPodWithClusterDNS(t *testing.T) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000123")
	rt := testRuntime()
	existing := pinnedPod(t, kubernetes.BuildPodSpec(run, rt, run.Status.Identity.ServiceAccountName), rt)
	existing.Spec.DNSPolicy, existing.Spec.DNSConfig = corev1.DNSClusterFirst, nil
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t), existing).Build()
	if _, err := newSandboxClient(c).Ensure(context.Background(), run, rt); err == nil || !strings.Contains(err.Error(), "dns") {
		t.Fatalf("Ensure error = %v, want a dns mismatch", err)
	}
}

// A retry that resolves the gateway to another address must still accept the
// controller's own Pod: the address it was created with is the one it uses.
func TestEnsureSandboxKeepsAddressPinnedAtCreation(t *testing.T) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000124")
	rt := testRuntime()
	existing := pinnedPod(t, kubernetes.BuildPodSpec(run, rt, run.Status.Identity.ServiceAccountName), rt)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t), existing).Build()
	moved := func(context.Context, string) (string, error) { return "10.96.0.99", nil }
	if _, err := kubernetes.NewPodSandboxClient(c, c).WithGatewayResolver(moved).Ensure(context.Background(), run, rt); err != nil {
		t.Fatalf("Ensure rejected its own Pod after the gateway address changed: %v", err)
	}
	other := existing.DeepCopy()
	other.Spec.HostAliases[0].Hostnames = []string{"attacker.example"}
	c = fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t), other).Build()
	if _, err := newSandboxClient(c).Ensure(context.Background(), run, rt); err == nil {
		t.Fatal("Ensure accepted a Pod that pins a different host")
	}
}

// Re-checking an existing sandbox must not need the resolver: an outage would
// stop the controller observing the Pod it already created.
func TestEnsureSandboxAdoptsExistingPodWithoutResolving(t *testing.T) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000125")
	rt := testRuntime()
	existing := pinnedPod(t, kubernetes.BuildPodSpec(run, rt, run.Status.Identity.ServiceAccountName), rt)
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t), existing).Build()
	outage := func(context.Context, string) (string, error) { return "", errors.New("resolver unavailable") }
	if _, err := kubernetes.NewPodSandboxClient(c, c).WithGatewayResolver(outage).Ensure(context.Background(), run, rt); err != nil {
		t.Fatalf("Ensure needed the resolver for an existing sandbox: %v", err)
	}
}

// A run-named Pod whose identity labels changed is neither deleted nor
// reported released: release fails loudly so someone looks at it.
func TestDeleteForRunRefusesTamperedSandbox(t *testing.T) {
	run := testRunForSandbox("cccccccc-0000-0000-0000-000000000126")
	rt := testRuntime()
	pod := pinnedPod(t, kubernetes.BuildPodSpec(run, rt, run.Status.Identity.ServiceAccountName), rt)
	pod.Labels[kubernetes.RunUIDLabel] = "someone-else"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(pod).Build()
	err := newSandboxClient(c).DeleteForRun(context.Background(), pod.Name, string(run.UID))
	if !errors.Is(err, kubernetes.ErrSandboxTampered) {
		t.Fatalf("DeleteForRun = %v, want ErrSandboxTampered", err)
	}
	var still corev1.Pod
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), &still); err != nil {
		t.Fatalf("tampered Pod was deleted: %v", err)
	}
}
