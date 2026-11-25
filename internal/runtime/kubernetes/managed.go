package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/deadline"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/sandboxpod"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CaptureContainer is a managed session's checkpoint helper.
const CaptureContainer = sandboxpod.CaptureContainer

// ManagedWriterFenceFinalizer retains kubelet termination evidence until the
// trusted placement has persisted the exact native container's physical fence.
const ManagedWriterFenceFinalizer = "sessions.sproozi.com/writer-fence"
const nativeConfigKey = "native.json"
const checkpointTemporaryVolume = "checkpoint-tmp"
const workspaceDirectory = "/workspace"
const workspaceVolume = "workspace"
const managedTemplateEnabledValue = "true"

func addManagedNativeContract(ctx context.Context, kube client.Client, run *api.AgentRun, template *api.AgentTemplate,
	runtime *api.AgentRuntime, contract *corev1.ConfigMap,
) error {
	binding, err := registry.ResolveRun(ctx, kube, run, template)
	if err != nil || binding == nil {
		return err
	}
	if err := ValidateManagedHarness(&runtime.Spec, binding.Harness); err != nil {
		return err
	}
	thread := binding.NativeUUID
	if binding.WorkspaceDigest != "" {
		// Initial contracts retain empty identity even after the genuine native
		// UUID is bound, so controller retries never rewrite immutable config.
		thread = ""
	}
	signIn := runtime.Spec.ClientConfig.SignIn != nil
	credential := harness.CredentialKey
	switch native := runtime.Spec.ClientConfig.NativeSession; {
	case signIn:
		credential = harness.CredentialSignIn
	case native != nil && native.SignInToken:
		credential = harness.CredentialToken
	}
	env, err := harness.NativeBaseEnvironment(binding.Harness, string(run.UID), credential)
	if err != nil {
		return err
	}
	clients, err := harness.NativeClients(binding.Harness, run.Spec.Capabilities, runtime.Spec.GatewayEndpoint)
	if err != nil {
		return err
	}
	launch := harness.NativeLaunch{Harness: binding.Harness, Deadline: binding.OriginalDeadline, ThreadID: thread, Task: binding.Task,
		Instructions: template.Spec.Instructions, InstructionDigest: binding.InstructionDigest, RolloutMaxBytes: harness.NativeRolloutMaxBytes,
		Env: env, ClientConfig: clients,
		Gateway: &harness.NativeGateway{Endpoint: runtime.Spec.GatewayEndpoint,
			TokenPath: harness.NativeGatewayTokenPath, TrustBundle: harness.NativeGatewayTrustBundle}}
	if native := runtime.Spec.ClientConfig.NativeSession; native != nil {
		launch.Model, launch.Options = native.Model, slices.Clone(native.Options)
	}
	if signIn {
		launch.SignInDirectory = api.SignInDirectory
	}
	if err = launch.Validate(time.Now()); err != nil {
		return err
	}
	document, err := json.Marshal(launch)
	if err != nil {
		return err
	}
	contract.Data[nativeConfigKey] = string(document)
	// The executor stored the profile beside the registration; the controller
	// re-checks its digest before the agent can read it.
	if binding.ProfileDigest != "" {
		archive, err := registry.ReadProfile(ctx, kube, run.Namespace, binding.ProfileDigest)
		if err != nil {
			return fmt.Errorf("managed native profile: %w", err)
		}
		contract.BinaryData = map[string][]byte{registry.ProfileArchiveKey: archive}
	}
	return nil
}

// ManagedSandboxClient provisions an inert native supervisor and a separate
// trusted storage helper. Ordinary SandboxClient implementations cannot silently
// ignore a required session binding.
type ManagedSandboxClient interface {
	EnsureManaged(context.Context, *api.AgentRun, *api.AgentRuntime, *registry.Record) (string, error)
}

// EnsureManaged creates the managed sandbox Pod. rt must already have passed
// ValidateRuntime.
func (p *PodSandboxClient) EnsureManaged(ctx context.Context, run *api.AgentRun, rt *api.AgentRuntime, binding *registry.Record) (string, error) {
	pod, err := BuildManagedPodSpec(run, rt, binding)
	if err != nil {
		return "", err
	}
	return p.ensurePod(ctx, pod, rt)
}

// useSessionLogin mounts the session's own named login's claim.
func useSessionLogin(pod *corev1.Pod, binding *registry.Record) {
	for i := range pod.Spec.Volumes {
		if v := &pod.Spec.Volumes[i]; v.Name == signInVolume && v.PersistentVolumeClaim != nil {
			v.PersistentVolumeClaim.ClaimName = LoginClaim(string(binding.Harness), binding.Login)
		}
	}
}

// BuildsManagedPod reports whether a managed Pod may still be built for a run
// bound to the registration r: BuildManagedPodSpec refuses one that is
// creating, cancelled or stopped. Only then does the run's admission check the
// runtime against the managed rules; a completed stop ends its run without a
// Pod, whatever the runtime says now.
func BuildsManagedPod(r *registry.Record) bool {
	return r != nil && r.Phase != registry.Creating && r.Phase != registry.Cancelled && r.Phase != registry.Stopped
}

// ValidateManagedRuntime checks a runtime against the managed rules for a
// session of the given harness: ValidateManagedHarness, then
// ValidateManagedShape. Run admission and the remote executor's placement
// apply it; building the agent contract and the Pod apply its parts.
func ValidateManagedRuntime(spec *api.AgentRuntimeSpec, registered harness.NativeKind) error {
	if err := ValidateManagedHarness(spec, registered); err != nil {
		return err
	}
	return ValidateManagedShape(spec)
}

// ValidateManagedHarness checks that the runtime names the session's harness,
// which must itself be a valid one. It is the one copy of this rule: the
// managed agent contract and Pod refuse a runtime that fails it.
func ValidateManagedHarness(spec *api.AgentRuntimeSpec, registered harness.NativeKind) error {
	if registered.Validate() != nil || spec.ClientConfig.Harness != string(registered) {
		return fmt.Errorf("managed runtime harness %q does not match the session's harness %q",
			spec.ClientConfig.Harness, registered)
	}
	return nil
}

// ValidateManagedShape checks the Pod template shape a managed session needs:
// exactly two containers, the agent (the only workload container) and the
// checkpoint helper; a helper with no environment, mounts, devices, lifecycle
// hooks or ports; and no template volume with the reserved checkpoint
// temporary volume's name. It is the one copy of these rules: building a
// managed Pod refuses a template that fails it.
func ValidateManagedShape(spec *api.AgentRuntimeSpec) error {
	if len(spec.WorkloadContainers) != 1 || spec.WorkloadContainers[0] != agentContainerName ||
		len(spec.PodTemplate.Spec.Containers) != 2 {
		return fmt.Errorf("managed runtime requires one native agent and one checkpoint helper")
	}
	helper := slices.IndexFunc(spec.PodTemplate.Spec.Containers, func(c corev1.Container) bool { return c.Name == CaptureContainer })
	if helper < 0 {
		return fmt.Errorf("managed runtime checkpoint helper is missing")
	}
	configured := spec.PodTemplate.Spec.Containers[helper]
	if len(configured.Env) != 0 || len(configured.EnvFrom) != 0 || len(configured.VolumeMounts) != 0 ||
		len(configured.VolumeDevices) != 0 || configured.Lifecycle != nil || len(configured.Ports) != 0 {
		return fmt.Errorf("checkpoint helper cannot receive credentials, mounts or workload hooks")
	}
	for _, volume := range spec.PodTemplate.Spec.Volumes {
		if volume.Name == checkpointTemporaryVolume {
			return fmt.Errorf("checkpoint temporary volume is reserved")
		}
	}
	return nil
}

func BuildManagedPodSpec(run *api.AgentRun, rt *api.AgentRuntime, binding *registry.Record) (*corev1.Pod, error) {
	if binding == nil {
		return nil, fmt.Errorf("managed sandbox requires a native registration")
	}
	if err := ValidateManagedHarness(&rt.Spec, binding.Harness); err != nil {
		return nil, err
	}
	if binding.RunUID != string(run.UID) || binding.IntendedRunName != run.Name ||
		binding.TemplateName != run.Spec.TemplateRef.Name || deadline.At(binding.OriginalDeadline).Passed(time.Now()) ||
		!BuildsManagedPod(binding) {
		return nil, fmt.Errorf("managed sandbox requires a current exact registration")
	}
	if err := ValidateManagedShape(&rt.Spec); err != nil {
		return nil, err
	}
	pod := BuildPodSpec(run, rt, run.Status.Identity.ServiceAccountName)
	useSessionLogin(pod, binding)
	pod.Finalizers = append(pod.Finalizers, ManagedWriterFenceFinalizer)
	pod.Annotations = map[string]string{
		"sessions.sproozi.com/registry-uid": binding.Reference.UID,
		"sessions.sproozi.com/epoch":        strconv.FormatUint(binding.Epoch, 10),
	}
	size := resource.MustParse("16Mi")
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: checkpointTemporaryVolume,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &size}}})
	// Memory-backed EmptyDirs apply the size limit at the filesystem rather than
	// relying on periodic kubelet disk-usage eviction after untrusted extraction.
	for i := range pod.Spec.Volumes {
		if volume := &pod.Spec.Volumes[i]; volume.EmptyDir != nil {
			volume.EmptyDir.Medium = corev1.StorageMediumMemory
		}
	}
	for i := range pod.Spec.Containers {
		container := &pod.Spec.Containers[i]
		switch container.Name {
		case agentContainerName:
			container.Command = []string{"/usr/local/bin/native-session"}
			container.Args = []string{"serve", "--config", "/etc/sproozi/native/config.json"}
			container.WorkingDir = workspaceDirectory
			container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: contractVolumeName,
				MountPath: "/etc/sproozi/native/config.json", SubPath: nativeConfigKey, ReadOnly: true})
			if binding.ProfileDigest != "" {
				container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{Name: contractVolumeName,
					MountPath: harness.NativeProfileArchive, SubPath: registry.ProfileArchiveKey, ReadOnly: true})
			}
		case CaptureContainer:
			container.Command = []string{"/usr/local/bin/checkpoint"}
			container.Args = []string{"hold", "--deadline", deadline.At(binding.OriginalDeadline).String()}
			container.WorkingDir = workspaceDirectory
			container.VolumeMounts = []corev1.VolumeMount{{Name: workspaceVolume, MountPath: workspaceDirectory},
				{Name: checkpointTemporaryVolume, MountPath: "/tmp"}}
		default:
			return nil, fmt.Errorf("managed runtime contains an unexpected container")
		}
	}
	return pod, nil
}
