package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const CaptureContainer = "checkpoint"

// ManagedWriterFenceFinalizer retains kubelet termination evidence until the
// trusted placement has persisted the exact native container's physical fence.
const ManagedWriterFenceFinalizer = "sessions.sproozi.com/writer-fence"
const nativeConfigKey = "native.json"
const checkpointTemporaryVolume = "checkpoint-tmp"
const workspaceDirectory = "/workspace"
const workspaceVolume = "workspace"
const managedTemplateEnabledValue = "true"
const openAIAPIKey = "OPENAI_API_KEY"

func addManagedNativeContract(ctx context.Context, kube client.Client, run *api.AgentRun, template *api.AgentTemplate,
	runtime *api.AgentRuntime, contract *corev1.ConfigMap,
) error {
	binding, err := registry.ResolveRun(ctx, kube, run, template)
	if err != nil || binding == nil {
		return err
	}
	if err := binding.Harness.Validate(); err != nil || runtime.Spec.ClientConfig.Harness != string(binding.Harness) {
		return fmt.Errorf("managed native runtime does not match registered harness")
	}
	thread := binding.NativeUUID
	if binding.WorkspaceDigest != "" {
		// Initial contracts retain empty identity even after the genuine native
		// UUID is bound, so controller retries never rewrite immutable config.
		thread = ""
	}
	env, err := harness.NativeBaseEnvironment(binding.Harness, string(run.UID), false)
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
	return p.ensurePod(ctx, pod, rt.Spec.GatewayEndpoint)
}

func BuildManagedPodSpec(run *api.AgentRun, rt *api.AgentRuntime, binding *registry.Record) (*corev1.Pod, error) {
	if binding == nil {
		return nil, fmt.Errorf("managed sandbox requires a native registration")
	}
	if err := binding.Harness.Validate(); err != nil || rt.Spec.ClientConfig.Harness != string(binding.Harness) {
		return nil, fmt.Errorf("managed sandbox native harness mismatch")
	}
	if binding.RunUID != string(run.UID) || binding.IntendedRunName != run.Name ||
		binding.TemplateName != run.Spec.TemplateRef.Name || !binding.OriginalDeadline.After(time.Now()) ||
		binding.Phase == registry.Creating || binding.Phase == registry.Cancelled || binding.Phase == registry.Stopped {
		return nil, fmt.Errorf("managed sandbox requires a current exact registration")
	}
	if len(rt.Spec.WorkloadContainers) != 1 || rt.Spec.WorkloadContainers[0] != agentContainerName ||
		len(rt.Spec.PodTemplate.Spec.Containers) != 2 {
		return nil, fmt.Errorf("managed runtime requires one native agent and one checkpoint helper")
	}
	helper := slices.IndexFunc(rt.Spec.PodTemplate.Spec.Containers, func(c corev1.Container) bool { return c.Name == CaptureContainer })
	if helper < 0 {
		return nil, fmt.Errorf("managed runtime checkpoint helper is missing")
	}
	configured := rt.Spec.PodTemplate.Spec.Containers[helper]
	if len(configured.Env) != 0 || len(configured.EnvFrom) != 0 || len(configured.VolumeMounts) != 0 ||
		len(configured.VolumeDevices) != 0 || configured.Lifecycle != nil || len(configured.Ports) != 0 {
		return nil, fmt.Errorf("checkpoint helper cannot receive credentials, mounts or workload hooks")
	}
	for _, volume := range rt.Spec.PodTemplate.Spec.Volumes {
		if volume.Name == checkpointTemporaryVolume {
			return nil, fmt.Errorf("checkpoint temporary volume is reserved")
		}
	}
	pod := BuildPodSpec(run, rt, run.Status.Identity.ServiceAccountName)
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
			container.Args = []string{"hold", "--deadline", binding.OriginalDeadline.Format(time.RFC3339Nano)}
			container.WorkingDir = workspaceDirectory
			container.VolumeMounts = []corev1.VolumeMount{{Name: workspaceVolume, MountPath: workspaceDirectory},
				{Name: checkpointTemporaryVolume, MountPath: "/tmp"}}
		default:
			return nil, fmt.Errorf("managed runtime contains an unexpected container")
		}
	}
	return pod, nil
}
