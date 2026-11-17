package kubernetes_test

import (
	"context"
	"path"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
)

const (
	testGatewayTokenDirectory = "/var/run/sproozi/tokens/gateway"
	testTokenFile             = "token"
)

// The mount every workload container gets: the projected volume's directory,
// so the kubelet's refreshed token reaches the container.
var currentGatewayTokenMount = corev1.VolumeMount{Name: testGatewayTokenVolumeName, MountPath: testGatewayTokenDirectory, ReadOnly: true}

// The mount a controller before this one built. Kubernetes never updates a
// projected file mounted through subPath, so its token was never refreshed.
var earlierGatewayTokenMount = corev1.VolumeMount{Name: testGatewayTokenVolumeName,
	MountPath: harness.NativeGatewayTokenPath, SubPath: testTokenFile, ReadOnly: true}

// sandboxCase is one sandbox Pod as the controller builds it, with the inputs
// Ensure builds it from.
type sandboxCase struct {
	run     *api.AgentRun
	runtime *api.AgentRuntime
	binding *registry.Record
	pod     *corev1.Pod
}

// builtPods returns an ordinary and a managed sandbox Pod.
func builtPods(t *testing.T) map[string]sandboxCase {
	t.Helper()
	run, runtime, binding := managedFixture()
	managed, err := kubernetes.BuildManagedPodSpec(run, runtime, binding)
	if err != nil {
		t.Fatal(err)
	}
	ordinary := testRuntime()
	return map[string]sandboxCase{
		"ordinary": {run: run, runtime: ordinary, pod: kubernetes.BuildPodSpec(run, ordinary, run.Status.Identity.ServiceAccountName)},
		"managed":  {run: run, runtime: runtime, binding: binding, pod: managed},
	}
}

// The kubelet updates a projected volume only through a directory mount, so
// the gateway token's directory is mounted, the token keeps the path every
// reader uses, and the directory holds nothing but the token.
func TestBuildPodSpecMountsTheGatewayTokenDirectory(t *testing.T) {
	for name, built := range builtPods(t) {
		t.Run(name, func(t *testing.T) {
			pod := built.pod
			var tokenVolume *corev1.Volume
			for i := range pod.Spec.Volumes {
				if pod.Spec.Volumes[i].Name == testGatewayTokenVolumeName {
					tokenVolume = &pod.Spec.Volumes[i]
				}
			}
			if tokenVolume == nil || tokenVolume.Projected == nil || len(tokenVolume.Projected.Sources) != 1 {
				t.Fatalf("gateway token volume = %+v, want one projected source", tokenVolume)
			}
			source := tokenVolume.Projected.Sources[0]
			if source.ServiceAccountToken == nil || source.ConfigMap != nil || source.Secret != nil || source.DownwardAPI != nil ||
				source.ClusterTrustBundle != nil || source.PodCertificate != nil {
				t.Fatalf("gateway token volume projects %+v, want only the ServiceAccount token", source)
			}
			if mode := tokenVolume.Projected.DefaultMode; mode == nil || *mode != 0o444 {
				t.Fatalf("gateway token mode = %v, want 0444", mode)
			}
			if got := path.Join(testGatewayTokenDirectory, source.ServiceAccountToken.Path); got != harness.NativeGatewayTokenPath {
				t.Fatalf("token path = %s, want %s", got, harness.NativeGatewayTokenPath)
			}
			for _, c := range pod.Spec.Containers {
				var tokenMounts []corev1.VolumeMount
				for _, m := range c.VolumeMounts {
					if m.Name == testGatewayTokenVolumeName || m.MountPath == testGatewayTokenDirectory ||
						strings.HasPrefix(m.MountPath, testGatewayTokenDirectory+"/") {
						tokenMounts = append(tokenMounts, m)
					}
				}
				want := []corev1.VolumeMount{currentGatewayTokenMount}
				if c.Name == kubernetes.CaptureContainer {
					want = nil
				}
				if !reflect.DeepEqual(tokenMounts, want) {
					t.Errorf("container %s token mounts = %+v, want %+v", c.Name, tokenMounts, want)
				}
			}
		})
	}
}

// withEarlierTokenMount replaces the current gateway token mount in pod's
// containers with the earlier subPath form, as an earlier controller built it.
func withEarlierTokenMount(t *testing.T, pod *corev1.Pod) *corev1.Pod {
	t.Helper()
	replaced := 0
	for i := range pod.Spec.Containers {
		for j := range pod.Spec.Containers[i].VolumeMounts {
			if reflect.DeepEqual(pod.Spec.Containers[i].VolumeMounts[j], currentGatewayTokenMount) {
				pod.Spec.Containers[i].VolumeMounts[j] = earlierGatewayTokenMount
				replaced++
			}
		}
	}
	if replaced != 1 {
		t.Fatalf("replaced %d gateway token mounts, want 1", replaced)
	}
	return pod
}

// A sandbox Pod an earlier controller created but whose run never recorded it
// mounts the gateway token through subPath. The controller refuses it like any
// other difference from its own build; the run retries until it is cancelled.
func TestEnsureSandboxRefusesAPodWithTheEarlierGatewayTokenMount(t *testing.T) {
	for name, built := range builtPods(t) {
		t.Run(name, func(t *testing.T) {
			current := pinnedPod(t, built.pod.DeepCopy(), built.runtime)
			if err := ensureExisting(t, built, current); err != nil {
				t.Fatalf("Ensure refused the controller's own sandbox: %v", err)
			}
			earlier := withEarlierTokenMount(t, pinnedPod(t, built.pod.DeepCopy(), built.runtime))
			err := ensureExisting(t, built, earlier)
			if err == nil || !strings.Contains(err.Error(), "unsafe Pod settings: containers") {
				t.Fatalf("Ensure error = %v, want unsafe Pod settings: containers", err)
			}
		})
	}
}

// ensureExisting stores existing and ensures built's sandbox against it.
func ensureExisting(t *testing.T, built sandboxCase, existing *corev1.Pod) error {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t), existing).Build()
	client := newSandboxClient(c)
	if built.binding != nil {
		_, err := client.EnsureManaged(context.Background(), built.run, built.runtime, built.binding)
		return err
	}
	_, err := client.Ensure(context.Background(), built.run, built.runtime)
	return err
}
