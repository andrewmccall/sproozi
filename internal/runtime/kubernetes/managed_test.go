package kubernetes_test

import (
	"context"
	"errors"
	"fmt"
	"maps"
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
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
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

// Admission applies the managed rules exactly in the registration phases for
// which BuildManagedPodSpec still builds a Pod.
func TestBuildsManagedPodMatchesThePodBuilder(t *testing.T) {
	if kubernetes.BuildsManagedPod(nil) {
		t.Fatal("an ordinary run builds a managed Pod")
	}
	want := map[registry.Phase]bool{registry.Creating: false, registry.Bound: true, registry.Prepared: true,
		registry.Active: true, registry.Stopping: true, registry.Stopped: false, registry.Cancelled: false}
	for phase, builds := range want {
		run, runtime, binding := managedFixture()
		binding.Phase = phase
		_, err := kubernetes.BuildManagedPodSpec(run, runtime, binding)
		if got := kubernetes.BuildsManagedPod(binding); got != builds || (err == nil) != builds {
			t.Errorf("phase %s: BuildsManagedPod = %v, BuildManagedPodSpec error = %v; want builds = %v", phase, got, err, builds)
		}
	}
}

type managedShapeViolation struct {
	mutate func(*api.AgentRuntime)
	want   string
}

// managedShapeViolations breaks each managed-shape rule once, with a template
// that still passes the ordinary template check, and names the refusal.
func managedShapeViolations() map[string]managedShapeViolation {
	const (
		count   = "managed runtime requires one native agent and one checkpoint helper"
		missing = "managed runtime checkpoint helper is missing"
		helper  = "checkpoint helper cannot receive credentials, mounts or workload hooks"
		volume  = "checkpoint temporary volume is reserved"
	)
	containers := func(rt *api.AgentRuntime) []corev1.Container { return rt.Spec.PodTemplate.Spec.Containers }
	return map[string]managedShapeViolation{
		"extra container": {func(rt *api.AgentRuntime) {
			extra := *containers(rt)[0].DeepCopy()
			extra.Name = "other"
			rt.Spec.PodTemplate.Spec.Containers = append(rt.Spec.PodTemplate.Spec.Containers, extra)
		}, count},
		"agent only": {func(rt *api.AgentRuntime) {
			rt.Spec.PodTemplate.Spec.Containers = rt.Spec.PodTemplate.Spec.Containers[:1]
		}, count},
		"helper is a workload": {func(rt *api.AgentRuntime) {
			rt.Spec.WorkloadContainers = append(rt.Spec.WorkloadContainers, kubernetes.CaptureContainer)
		}, count},
		"workload is not the agent": {func(rt *api.AgentRuntime) {
			containers(rt)[0].Name = "worker"
			rt.Spec.WorkloadContainers = []string{"worker"}
		}, count},
		"helper missing": {func(rt *api.AgentRuntime) { containers(rt)[1].Name = "helper" }, missing},
		"helper env": {func(rt *api.AgentRuntime) {
			containers(rt)[1].Env = []corev1.EnvVar{{Name: "SECRET", Value: "forbidden"}}
		}, helper},
		"helper mount": {func(rt *api.AgentRuntime) {
			containers(rt)[1].VolumeMounts = []corev1.VolumeMount{{Name: "scratch", MountPath: "/scratch"}}
		}, helper},
		"helper device": {func(rt *api.AgentRuntime) {
			containers(rt)[1].VolumeDevices = []corev1.VolumeDevice{{Name: "disk", DevicePath: "/dev/disk"}}
		}, helper},
		"helper hook": {func(rt *api.AgentRuntime) {
			containers(rt)[1].Lifecycle = &corev1.Lifecycle{PreStop: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"true"}}}}
		}, helper},
		"helper port": {func(rt *api.AgentRuntime) {
			containers(rt)[1].Ports = []corev1.ContainerPort{{ContainerPort: 8080}}
		}, helper},
		"reserved volume": {func(rt *api.AgentRuntime) {
			size := resource.MustParse("1Mi")
			rt.Spec.PodTemplate.Spec.Volumes = append(rt.Spec.PodTemplate.Spec.Volumes, corev1.Volume{Name: "checkpoint-tmp",
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &size}}})
		}, volume},
	}
}

// managedHarnessViolations break the harness rule: the runtime names another
// harness than the registration, or none.
func managedHarnessViolations() map[string]managedShapeViolation {
	const want = `managed runtime harness %q does not match the session's harness "codex"`
	return map[string]managedShapeViolation{
		"another harness": {func(rt *api.AgentRuntime) { rt.Spec.ClientConfig.Harness = string(harness.NativeClaude) },
			fmt.Sprintf(want, harness.NativeClaude)},
		"no harness": {func(rt *api.AgentRuntime) { rt.Spec.ClientConfig.Harness = "" }, fmt.Sprintf(want, "")},
	}
}

// Building a managed Pod and admitting a managed run refuse exactly what
// ValidateManagedRuntime refuses, for the same reason: the rules have one
// copy, so admission cannot pass a runtime that EnsureManaged then refuses. An
// ordinary run's admission, and a stopped session's, do not apply them.
func TestManagedRulesHaveOneSource(t *testing.T) {
	run, runtime, binding := managedFixture()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(testTrustConfigMap(t)).Build()
	admitters := map[string]kubernetes.SandboxClient{"Pod": newSandboxClient(c), "fake": &kubernetes.FakeSandboxClient{}}
	if err := kubernetes.ValidateManagedRuntime(&runtime.Spec, binding.Harness); err != nil {
		t.Fatalf("fixture managed runtime refused: %v", err)
	}
	for client, admitter := range admitters {
		if err := admitter.ValidateRuntime(context.Background(), runtime, binding); err != nil {
			t.Fatalf("%s admission refused the fixture managed runtime: %v", client, err)
		}
	}
	violations := managedShapeViolations()
	maps.Copy(violations, managedHarnessViolations())
	for name, violation := range violations {
		t.Run(name, func(t *testing.T) {
			run, runtime, binding := run.DeepCopy(), runtime.DeepCopy(), *binding
			violation.mutate(runtime)
			if err := runtime.Spec.ValidateRuntimeTemplate(); err != nil {
				t.Fatalf("violation also breaks the ordinary template check: %v", err)
			}
			rulesErr := kubernetes.ValidateManagedRuntime(&runtime.Spec, binding.Harness)
			_, buildErr := kubernetes.BuildManagedPodSpec(run, runtime, &binding)
			// Building refuses permanently (ErrInvalidRuntime), so a runtime
			// edited after admission ends the run rather than retrying.
			wrapped := kubernetes.ErrInvalidRuntime.Error() + ": " + violation.want
			if rulesErr == nil || rulesErr.Error() != violation.want ||
				!errors.Is(buildErr, kubernetes.ErrInvalidRuntime) || buildErr.Error() != wrapped {
				t.Fatalf("ValidateManagedRuntime = %v, BuildManagedPodSpec = %v, want %q and %q", rulesErr, buildErr, violation.want, wrapped)
			}
			stopped := binding
			stopped.Phase = registry.Stopped
			for client, admitter := range admitters {
				err := admitter.ValidateRuntime(context.Background(), runtime, &binding)
				if !errors.Is(err, kubernetes.ErrInvalidRuntime) || err.Error() != kubernetes.ErrInvalidRuntime.Error()+": "+violation.want {
					t.Fatalf("%s managed admission = %v, want ErrInvalidRuntime for %q", client, err, violation.want)
				}
				if err := admitter.ValidateRuntime(context.Background(), runtime, nil); err != nil {
					t.Fatalf("%s ordinary admission applied the managed rules: %v", client, err)
				}
				if err := admitter.ValidateRuntime(context.Background(), runtime, &stopped); err != nil {
					t.Fatalf("%s stopped session's admission applied the managed rules: %v", client, err)
				}
			}
		})
	}
}
