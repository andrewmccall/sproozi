package kubernetes_test

import (
	"bytes"
	"context"
	"path"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
)

const (
	testGatewayTokenDirectory = "/var/run/sproozi/tokens/gateway"
	unrefreshedTokenMessage   = "gateway token is never refreshed"
)

// The mount every workload container gets: the projected volume's directory,
// so the kubelet's refreshed token reaches the container.
var currentGatewayTokenMount = corev1.VolumeMount{Name: testGatewayTokenVolumeName, MountPath: testGatewayTokenDirectory, ReadOnly: true}

// The mount a controller before this one built. Kubernetes never updates a
// projected file mounted through subPath, so its token was never refreshed.
var earlierGatewayTokenMount = corev1.VolumeMount{Name: testGatewayTokenVolumeName,
	MountPath: harness.NativeGatewayTokenPath, SubPath: "token", ReadOnly: true}

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

// earlierPod returns built's Pod as an earlier controller created it.
func earlierPod(t *testing.T, built sandboxCase) *corev1.Pod {
	t.Helper()
	return withEarlierTokenMount(t, pinnedPod(t, built.pod.DeepCopy(), built.runtime))
}

// ensureExisting stores existing and ensures built's sandbox twice with one
// client, as repeated reconciliations do.
func ensureExisting(ctx context.Context, t *testing.T, built sandboxCase, existing *corev1.Pod) error {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t), existing).Build()
	client := newSandboxClient(c)
	for range 2 {
		var err error
		if built.binding != nil {
			_, err = client.EnsureManaged(ctx, built.run, built.runtime, built.binding)
		} else {
			_, err = client.Ensure(ctx, built.run, built.runtime)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// A sandbox Pod an earlier controller created differs from the current build
// only in the gateway token's subPath mount. Upgrading the controller adopts
// it, and says once that its gateway token is never refreshed.
func TestEnsureSandboxAdoptsAPodWithTheEarlierGatewayTokenMount(t *testing.T) {
	for name, built := range builtPods(t) {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			ctx := log.IntoContext(context.Background(), zap.New(zap.WriteTo(&logs)))
			if err := ensureExisting(ctx, t, built, earlierPod(t, built)); err != nil {
				t.Fatalf("Ensure refused a sandbox with the earlier gateway token mount: %v", err)
			}
			if got := strings.Count(logs.String(), unrefreshedTokenMessage); got != 1 {
				t.Fatalf("adoption logged the unrefreshed token %d times, want once:\n%s", got, logs.String())
			}

			logs.Reset()
			if err := ensureExisting(ctx, t, built, pinnedPod(t, built.pod.DeepCopy(), built.runtime)); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(logs.String(), unrefreshedTokenMessage) {
				t.Fatalf("adopting a current sandbox logged an unrefreshed token:\n%s", logs.String())
			}
		})
	}
}

// Adoption accepts exactly the earlier gateway token mount and nothing near it:
// the comparison is what keeps an arbitrary Pod from standing in for a sandbox.
func TestEnsureSandboxRefusesNearMissesOfTheEarlierGatewayTokenMount(t *testing.T) {
	tokenMount := func(pod *corev1.Pod) *corev1.VolumeMount {
		for i := range pod.Spec.Containers[0].VolumeMounts {
			if m := &pod.Spec.Containers[0].VolumeMounts[i]; m.Name == testGatewayTokenVolumeName {
				return m
			}
		}
		panic("no gateway token mount")
	}
	mountAt := func(pod *corev1.Pod, mountPath string) *corev1.VolumeMount {
		for i := range pod.Spec.Containers[0].VolumeMounts {
			if m := &pod.Spec.Containers[0].VolumeMounts[i]; m.MountPath == mountPath {
				return m
			}
		}
		panic("no mount at " + mountPath)
	}
	for _, tc := range []struct {
		name    string
		managed bool
		mutate  func(*corev1.Pod)
	}{
		{name: "writable", mutate: func(p *corev1.Pod) { tokenMount(p).ReadOnly = false }},
		{name: "other subPath", mutate: func(p *corev1.Pod) { tokenMount(p).SubPath = "..data/token" }},
		{name: "subPathExpr", mutate: func(p *corev1.Pod) { m := tokenMount(p); m.SubPath, m.SubPathExpr = "", "token" }},
		{name: "other path", mutate: func(p *corev1.Pod) { tokenMount(p).MountPath = "/home/agent/token" }},
		{name: "propagation", mutate: func(p *corev1.Pod) {
			tokenMount(p).MountPropagation = ptr.To(corev1.MountPropagationHostToContainer)
		}},
		{name: "recursive read-only", mutate: func(p *corev1.Pod) {
			tokenMount(p).RecursiveReadOnly = ptr.To(corev1.RecursiveReadOnlyDisabled)
		}},
		{name: "other volume", mutate: func(p *corev1.Pod) { tokenMount(p).Name = testContractVolumeName }},
		{name: "directory with subPath", mutate: func(p *corev1.Pod) { tokenMount(p).MountPath = testGatewayTokenDirectory }},
		{name: "both forms", mutate: func(p *corev1.Pod) {
			p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, currentGatewayTokenMount)
		}},
		{name: "earlier form twice", mutate: func(p *corev1.Pod) {
			p.Spec.Containers[0].VolumeMounts = append(p.Spec.Containers[0].VolumeMounts, earlierGatewayTokenMount)
		}},
		{name: "longer token lifetime", mutate: func(p *corev1.Pod) {
			for _, v := range p.Spec.Volumes {
				if v.Name == testGatewayTokenVolumeName {
					v.Projected.Sources[0].ServiceAccountToken.ExpirationSeconds = ptr.To(int64(48 * 60 * 60))
				}
			}
		}},
		{name: "subPath on the trust mount", mutate: func(p *corev1.Pod) {
			m := mountAt(p, "/etc/sproozi/trust")
			m.MountPath, m.SubPath = "/etc/sproozi/trust/ca-bundle.pem", "ca-bundle.pem"
		}},
		{name: "subPath on the workspace mount", mutate: func(p *corev1.Pod) { mountAt(p, "/workspace").SubPath = "token" }},
		{name: "token mounted into the helper", managed: true, mutate: func(p *corev1.Pod) {
			p.Spec.Containers[1].VolumeMounts = append(p.Spec.Containers[1].VolumeMounts, earlierGatewayTokenMount)
		}},
		{name: "managed agent writable", managed: true, mutate: func(p *corev1.Pod) { tokenMount(p).ReadOnly = false }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			built := builtPods(t)["ordinary"]
			if tc.managed {
				built = builtPods(t)["managed"]
			}
			existing := earlierPod(t, built)
			tc.mutate(existing)
			err := ensureExisting(context.Background(), t, built, existing)
			if err == nil || !strings.Contains(err.Error(), "unsafe Pod settings") {
				t.Fatalf("Ensure error = %v, want unsafe Pod settings", err)
			}
		})
	}
}
