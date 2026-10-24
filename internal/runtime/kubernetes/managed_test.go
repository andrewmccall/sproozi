package kubernetes_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	corev1 "k8s.io/api/core/v1"
)

func managedFixture() (*api.AgentRun, *api.AgentRuntime, *registry.Record) {
	run := testRunForSandbox("aaaaaaaa-0000-0000-0000-000000000001")
	run.Spec.TemplateRef.Name = "mobility"
	run.Status.Identity.ServiceAccountName = kubernetes.RunName(run.UID)
	runtime := testRuntime()
	runtime.Spec.ClientConfig.Harness = "codex"
	helper := *runtime.Spec.PodTemplate.Spec.Containers[0].DeepCopy()
	helper.Name = kubernetes.CaptureContainer
	runtime.Spec.PodTemplate.Spec.Containers = append(runtime.Spec.PodTemplate.Spec.Containers, helper)
	binding := &registry.Record{Reference: registry.Reference{UID: "registry-uid", Epoch: 2},
		Registration: registry.Registration{Harness: harness.NativeCodex, TemplateName: run.Spec.TemplateRef.Name, IntendedRunName: run.Name,
			OriginalDeadline: time.Now().UTC().Add(5 * time.Minute), Epoch: 2},
		RunUID: string(run.UID), Phase: registry.Bound}
	return run, runtime, binding
}

func TestManagedPodKeepsHelperOutsideWorkloadAuthority(t *testing.T) {
	run, runtime, binding := managedFixture()
	pod, err := kubernetes.BuildManagedPodSpec(run, runtime, binding)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pod.Finalizers, []string{kubernetes.ManagedWriterFenceFinalizer}) {
		t.Fatal("managed Pod did not retain exact writer-stop observation")
	}
	ordinary := kubernetes.BuildPodSpec(run, runtime, run.Status.Identity.ServiceAccountName)
	if len(ordinary.Finalizers) != 0 {
		t.Fatal("managed writer-fence finalizer leaked into ordinary Pods")
	}
	agent, helper := pod.Spec.Containers[0], pod.Spec.Containers[1]
	if !reflect.DeepEqual(agent.Command, []string{"/usr/local/bin/native-session"}) ||
		!reflect.DeepEqual(agent.Args, []string{"serve", "--config", "/etc/sproozi/native/config.json"}) {
		t.Fatal("native command was not fixed by the managed runtime")
	}
	if !reflect.DeepEqual(helper.Command, []string{"/usr/local/bin/checkpoint"}) ||
		!reflect.DeepEqual(helper.Args, []string{"hold", "--deadline", binding.OriginalDeadline.Format(time.RFC3339Nano)}) {
		t.Fatal("helper did not retain the original absolute deadline")
	}
	wanted := []corev1.VolumeMount{{Name: "workspace", MountPath: "/workspace"},
		{Name: "checkpoint-tmp", MountPath: "/tmp"}}
	if !reflect.DeepEqual(helper.VolumeMounts, wanted) || len(helper.Env) != 0 || len(helper.EnvFrom) != 0 {
		t.Fatal("helper received workload credentials, native tmp or launch configuration")
	}
	for _, volume := range pod.Spec.Volumes {
		if volume.EmptyDir != nil && (volume.EmptyDir.Medium != corev1.StorageMediumMemory ||
			volume.EmptyDir.SizeLimit == nil || volume.EmptyDir.SizeLimit.IsZero()) {
			t.Fatalf("temporary volume %s lacks a filesystem bound", volume.Name)
		}
	}
	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever || *pod.Spec.AutomountServiceAccountToken {
		t.Fatal("managed execution could restart or inherit Kubernetes credentials")
	}
}

func TestManagedPodRejectsWrongBindingOrHelperCredentialConfiguration(t *testing.T) {
	mutations := map[string]func(*api.AgentRuntime, *registry.Record){
		"wrong Run UID": func(_ *api.AgentRuntime, r *registry.Record) { r.RunUID = "replacement" },
		"expired":       func(_ *api.AgentRuntime, r *registry.Record) { r.OriginalDeadline = time.Now().Add(-time.Second) },
		"cancelled":     func(_ *api.AgentRuntime, r *registry.Record) { r.Phase = registry.Cancelled },
		"helper env": func(rt *api.AgentRuntime, _ *registry.Record) {
			rt.Spec.PodTemplate.Spec.Containers[1].Env = []corev1.EnvVar{{Name: "SECRET", Value: "forbidden"}}
		},
		"helper mount": func(rt *api.AgentRuntime, _ *registry.Record) {
			rt.Spec.PodTemplate.Spec.Containers[1].VolumeMounts = []corev1.VolumeMount{{Name: "secret", MountPath: "/secret"}}
		},
		"extra writer": func(rt *api.AgentRuntime, _ *registry.Record) {
			rt.Spec.PodTemplate.Spec.Containers = append(rt.Spec.PodTemplate.Spec.Containers, corev1.Container{Name: "other"})
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			run, runtime, binding := managedFixture()
			mutate(runtime, binding)
			if _, err := kubernetes.BuildManagedPodSpec(run, runtime, binding); err == nil {
				t.Fatal("accepted unsafe managed placement")
			}
		})
	}
}

func TestManagedPodNativeKindMustMatchRegistration(t *testing.T) {
	for _, kind := range []harness.NativeKind{harness.NativeCodex, harness.NativeClaude} {
		t.Run(string(kind), func(t *testing.T) {
			run, runtime, binding := managedFixture()
			binding.Harness = kind
			runtime.Spec.ClientConfig.Harness = string(kind)
			if _, err := kubernetes.BuildManagedPodSpec(run, runtime, binding); err != nil {
				t.Fatal(err)
			}
			runtime.Spec.ClientConfig.Harness = "different-client"
			if _, err := kubernetes.BuildManagedPodSpec(run, runtime, binding); err == nil {
				t.Fatal("mismatched runtime client admitted")
			}
			binding.Harness = ""
			if _, err := kubernetes.BuildManagedPodSpec(run, runtime, binding); err == nil {
				t.Fatal("empty kind selected Codex")
			}
		})
	}
}

// Only an agent whose registration names a profile mounts the archive, read-only.
func TestManagedPodMountsTheProfileArchiveOnlyWhenRegistered(t *testing.T) {
	run, runtime, binding := managedFixture()
	mounted := func() []corev1.VolumeMount {
		pod, err := kubernetes.BuildManagedPodSpec(run, runtime, binding)
		if err != nil {
			t.Fatal(err)
		}
		var found []corev1.VolumeMount
		for _, c := range pod.Spec.Containers {
			for _, m := range c.VolumeMounts {
				if m.SubPath == registry.ProfileArchiveKey {
					found = append(found, m)
				}
			}
		}
		return found
	}
	if got := mounted(); len(got) != 0 {
		t.Fatalf("profile mounted without registration: %+v", got)
	}
	binding.ProfileDigest = "sha256:" + strings.Repeat("0", 64)
	got := mounted()
	if len(got) != 1 || got[0].MountPath != "/etc/sproozi/profile-archive/profile.tar.gz" || !got[0].ReadOnly {
		t.Fatalf("profile mounts = %+v", got)
	}
}

// A managed session signs in with its own named login's claim (ADR 0049).
func TestManagedPodMountsItsSessionsNamedLogin(t *testing.T) {
	run, runtime, binding := managedFixture()
	runtime.Spec.ClientConfig.SignIn = &api.RuntimeSignIn{Claims: &api.RuntimeLoginClaims{}}
	for login, want := range map[string]string{"": "sproozi-login-codex-default", "work": "sproozi-login-codex-work"} {
		binding.Login = login
		pod, err := kubernetes.BuildManagedPodSpec(run, runtime, binding)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(pod.Spec.Volumes, func(v corev1.Volume) bool {
			return v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ClaimName == want
		}) {
			t.Fatalf("login %q: no claim %s in %+v", login, want, pod.Spec.Volumes)
		}
	}
}
