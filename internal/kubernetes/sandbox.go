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
package kubernetes

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"reflect"
	"slices"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

const (
	gatewayTokenPath    = "/var/run/sproozi/tokens/gateway/token"
	trustBundlePath     = "/etc/sproozi/trust/ca-bundle.pem"
	kubeconfigPath      = "/etc/sproozi/kubeconfig"
	maxTrustBundleBytes = 1 << 20
	// AgentPolicy caps executions at one hour. The runner currently reads the
	// projected token once when it configures its clients, so request a token
	// that covers that bound plus an equal allowance for scheduling and cleanup.
	// Gateway authorization still re-reads the live AgentRun on every request,
	// so cancellation and terminal-state revocation do not depend on token expiry.
	gatewayTokenExpirationSeconds int64 = 2 * 60 * 60
)

// SandboxStatus represents the observed lifecycle phase of a Sandbox Pod.
type SandboxStatus string

const (
	SandboxStatusPending   SandboxStatus = "Pending"
	SandboxStatusRunning   SandboxStatus = "Running"
	SandboxStatusSucceeded SandboxStatus = "Succeeded"
	SandboxStatusFailed    SandboxStatus = "Failed"
	// SandboxStatusUnknown is returned when the Pod does not exist.
	SandboxStatusUnknown SandboxStatus = "Unknown"
)

// SandboxClient abstracts Sandbox Pod lifecycle operations for unit-testable reconciliation.
type SandboxClient interface {
	// Ensure creates the Sandbox Pod if it does not already exist. Idempotent.
	// Returns the sandbox name (always RunName(run.UID)).
	Ensure(ctx context.Context, run *sprooziv1alpha1.AgentRun, rt *sprooziv1alpha1.AgentRuntime) (string, error)

	// GetStatus returns the current lifecycle status of the named Sandbox.
	// Returns SandboxStatusUnknown if the Pod does not exist.
	GetStatus(ctx context.Context, name string) (SandboxStatus, error)

	// GetExitCode returns the agent container's actual process exit code.
	// Only valid when GetStatus returns Succeeded or Failed.
	GetExitCode(ctx context.Context, name string) (int32, error)

	// Delete removes the named Sandbox from AgentsNamespace. NotFound is success.
	Delete(ctx context.Context, name string) error
}

// PodSandboxClient implements SandboxClient using a Kubernetes Pod.
type PodSandboxClient struct {
	c client.Client
}

// NewPodSandboxClient returns the Pod-backed execution adapter.
func NewPodSandboxClient(c client.Client) *PodSandboxClient {
	return &PodSandboxClient{c: c}
}

// Ensure creates the Sandbox Pod if it does not already exist. Idempotent.
func (p *PodSandboxClient) Ensure(ctx context.Context, run *sprooziv1alpha1.AgentRun, rt *sprooziv1alpha1.AgentRuntime) (string, error) {
	if err := rt.Spec.ValidateRuntimeTemplate(); err != nil {
		return "", fmt.Errorf("validate runtime template: %w", err)
	}
	if err := validateRuntimeTrustBundle(ctx, p.c, rt); err != nil {
		return "", err
	}
	saName := run.Status.Identity.ServiceAccountName
	pod := BuildPodSpec(run, rt, saName)
	if err := p.c.Create(ctx, pod); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", err
		}
		// Never treat a same-named object as ours. Names are derived from a run
		// UID, but an attacker or stale controller may have created an object
		// before this reconciliation. Accepting it would let an arbitrary Pod
		// replace the hardened sandbox.
		var existing corev1.Pod
		if getErr := p.c.Get(ctx, client.ObjectKeyFromObject(pod), &existing); getErr != nil {
			return "", getErr
		}
		if err := validateExistingSandbox(&existing, pod); err != nil {
			return "", err
		}
	}
	return pod.Name, nil
}

func validateExistingSandbox(existing, wanted *corev1.Pod) error {
	if existing.Namespace != AgentsNamespace || existing.Name != wanted.Name || !existing.DeletionTimestamp.IsZero() {
		return fmt.Errorf("existing sandbox %s is not an active run object", wanted.Name)
	}
	for key, value := range map[string]string{
		RunLabel:          wanted.Labels[RunLabel],
		RunNamespaceLabel: wanted.Labels[RunNamespaceLabel],
		RunUIDLabel:       wanted.Labels[RunUIDLabel],
		ManagedByLabel:    ManagedByValue,
	} {
		if existing.Labels[key] != value {
			return fmt.Errorf("existing sandbox %s has different run identity", wanted.Name)
		}
	}
	if existing.Spec.ServiceAccountName != wanted.Spec.ServiceAccountName ||
		existing.Spec.AutomountServiceAccountToken == nil || !reflect.DeepEqual(existing.Spec.AutomountServiceAccountToken, wanted.Spec.AutomountServiceAccountToken) ||
		existing.Spec.HostNetwork != wanted.Spec.HostNetwork || existing.Spec.HostPID != wanted.Spec.HostPID || existing.Spec.HostIPC != wanted.Spec.HostIPC ||
		existing.Spec.RestartPolicy != wanted.Spec.RestartPolicy ||
		!reflect.DeepEqual(existing.Spec.SecurityContext, wanted.Spec.SecurityContext) ||
		existing.Spec.RuntimeClassName != wanted.Spec.RuntimeClassName ||
		!apiequality.Semantic.DeepEqual(existing.Spec.Containers, wanted.Spec.Containers) ||
		!reflect.DeepEqual(existing.Spec.InitContainers, wanted.Spec.InitContainers) ||
		!reflect.DeepEqual(existing.Spec.EphemeralContainers, wanted.Spec.EphemeralContainers) ||
		!reflect.DeepEqual(existing.Spec.Volumes, wanted.Spec.Volumes) {
		return fmt.Errorf("existing sandbox %s has unsafe Pod settings", wanted.Name)
	}
	return nil
}

// GetStatus returns the current lifecycle status of the named Sandbox.
func (p *PodSandboxClient) GetStatus(ctx context.Context, name string) (SandboxStatus, error) {
	var pod corev1.Pod
	if err := p.c.Get(ctx, client.ObjectKey{Namespace: AgentsNamespace, Name: name}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return SandboxStatusUnknown, nil
		}
		return SandboxStatusUnknown, err
	}
	switch pod.Status.Phase {
	case corev1.PodPending:
		return SandboxStatusPending, nil
	case corev1.PodRunning:
		return SandboxStatusRunning, nil
	case corev1.PodSucceeded:
		return SandboxStatusSucceeded, nil
	case corev1.PodFailed:
		return SandboxStatusFailed, nil
	default:
		return SandboxStatusUnknown, nil
	}
}

// GetExitCode returns the agent container's actual process exit code.
func (p *PodSandboxClient) GetExitCode(ctx context.Context, name string) (int32, error) {
	var pod corev1.Pod
	if err := p.c.Get(ctx, client.ObjectKey{Namespace: AgentsNamespace, Name: name}, &pod); err != nil {
		return 0, err
	}

	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == "agent" && cs.State.Terminated != nil {
			return cs.State.Terminated.ExitCode, nil
		}
	}
	return 0, fmt.Errorf("agent container has not terminated")
}

// Delete removes the named Sandbox from AgentsNamespace. NotFound is success.
func (p *PodSandboxClient) Delete(ctx context.Context, name string) error {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: AgentsNamespace},
	}
	if err := p.c.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// DeleteForRun removes a sandbox only when its immutable run identity still
// matches. UID and resource-version preconditions make delete/recreate races
// fail safely rather than deleting a replacement Pod.
func (p *PodSandboxClient) DeleteForRun(ctx context.Context, name, runUID string) error {
	var pod corev1.Pod
	if err := p.c.Get(ctx, client.ObjectKey{Namespace: AgentsNamespace, Name: name}, &pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !ownedByRun(&pod, runUID) {
		return nil
	}
	if err := p.c.Delete(ctx, &pod, client.Preconditions{UID: &pod.UID, ResourceVersion: &pod.ResourceVersion}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// BuildPodSpec builds a Pod spec from the administrator-owned AgentRuntime and admitted identity.
// Run-controlled fields (image, security context, environment, pod template) are never
// accepted from the AgentRun spec. Untrusted task text is deliberately not injected into the
// environment; the runner will receive it through the versioned contract volume.
func BuildPodSpec(run *sprooziv1alpha1.AgentRun, rt *sprooziv1alpha1.AgentRuntime, saName string) *corev1.Pod {
	name := RunName(run.UID)

	sizeLimit := rt.Spec.EphemeralWorkspace.SizeLimit.DeepCopy()
	defaultMode := int32(0444)

	// Start with the administrator-owned template. AgentRun contributes only
	// identity and capability bindings below; it never supplies Pod structure.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: AgentsNamespace,
			Labels: map[string]string{
				RunLabel:          run.Name,
				RunNamespaceLabel: run.Namespace,
				RunUIDLabel:       string(run.UID),
				ManagedByLabel:    ManagedByValue,
			},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name: contractVolumeName, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: name},
					DefaultMode:          &defaultMode,
				}},
			}, {
				Name: "client-trust", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
					DefaultMode: &defaultMode,
					Sources: []corev1.VolumeProjection{
						{ConfigMap: &corev1.ConfigMapProjection{
							LocalObjectReference: corev1.LocalObjectReference{Name: rt.Spec.ClientConfig.TrustBundleConfigMap.Name},
							Items:                []corev1.KeyToPath{{Key: rt.Spec.ClientConfig.TrustBundleConfigMap.Key, Path: "ca-bundle.pem"}},
						}},
					},
				}},
			}, {
				Name: "workspace",
				VolumeSource: corev1.VolumeSource{
					EmptyDir: &corev1.EmptyDirVolumeSource{
						SizeLimit: &sizeLimit,
					},
				},
			}, {
				Name: "tmp", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &sizeLimit}},
			}, {
				Name: "agent-home", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{SizeLimit: &sizeLimit}},
			}, {
				Name: gatewayTokenVolumeName, VolumeSource: tokenVolumeSource("sproozi-gateway", defaultMode),
			}},
		},
	}
	injectedVolumes := pod.Spec.Volumes
	pod.Spec = *rt.Spec.PodTemplate.Spec.DeepCopy()
	pod.Spec.ServiceAccountName = saName
	pod.Spec.AutomountServiceAccountToken = ptrTo(false)
	pod.Spec.HostNetwork, pod.Spec.HostPID, pod.Spec.HostIPC = false, false, false
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	pod.Spec.Volumes = append(pod.Spec.Volumes, injectedVolumes...)

	if rt.Spec.RuntimeClassName != "" {
		pod.Spec.RuntimeClassName = &rt.Spec.RuntimeClassName
	}
	workload := rt.Spec.WorkloadContainers
	if len(workload) == 0 {
		workload = []string{"agent"}
	}
	workloadSet := make(map[string]bool, len(workload))
	for _, name := range workload {
		workloadSet[name] = true
	}
	for i := range pod.Spec.Containers {
		if !workloadSet[pod.Spec.Containers[i].Name] {
			continue
		}
		c := &pod.Spec.Containers[i]
		c.Env = append(c.Env,
			corev1.EnvVar{Name: "SPROOZI_GATEWAY", Value: rt.Spec.GatewayEndpoint},
			corev1.EnvVar{Name: "OPENAI_API_KEY", Value: clientAuthPlaceholder},
			corev1.EnvVar{Name: "OPENAI_BASE_URL", Value: "https://api.openai.com/v1"},
			corev1.EnvVar{Name: "SSL_CERT_FILE", Value: trustBundlePath},
			corev1.EnvVar{Name: "GIT_SSL_CAINFO", Value: trustBundlePath},
			corev1.EnvVar{Name: "NODE_EXTRA_CA_CERTS", Value: trustBundlePath},
			corev1.EnvVar{Name: "KUBECONFIG", Value: kubeconfigPath},
			corev1.EnvVar{Name: "GH_HOST", Value: "github.com"},
			corev1.EnvVar{Name: "GH_TOKEN", Value: clientAuthPlaceholder},
			corev1.EnvVar{Name: "GH_PROMPT_DISABLED", Value: "1"},
			corev1.EnvVar{Name: "GH_NO_UPDATE_NOTIFIER", Value: "1"},
			corev1.EnvVar{Name: "GIT_TERMINAL_PROMPT", Value: "0"},
		)
		c.VolumeMounts = append(c.VolumeMounts,
			corev1.VolumeMount{Name: contractVolumeName, MountPath: "/etc/sproozi/contract", ReadOnly: true},
			corev1.VolumeMount{Name: contractVolumeName, MountPath: kubeconfigPath, SubPath: kubeconfigConfigMapKey, ReadOnly: true},
			corev1.VolumeMount{Name: "client-trust", MountPath: "/etc/sproozi/trust", ReadOnly: true},
			corev1.VolumeMount{Name: "workspace", MountPath: "/workspace"}, corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"},
			corev1.VolumeMount{Name: "agent-home", MountPath: "/home/agent"},
			corev1.VolumeMount{Name: gatewayTokenVolumeName, MountPath: gatewayTokenPath, SubPath: "token", ReadOnly: true})
	}

	// Make API defaults explicit so re-reconciliation compares the admitted
	// Pod against the same canonical shape, without ignoring security fields.
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if c.ImagePullPolicy == "" {
			c.ImagePullPolicy = corev1.PullIfNotPresent
		}
		if c.TerminationMessagePath == "" {
			c.TerminationMessagePath = corev1.TerminationMessagePathDefault
		}
		if c.TerminationMessagePolicy == "" {
			c.TerminationMessagePolicy = corev1.TerminationMessageReadFile
		}
		if c.Resources.Requests == nil {
			c.Resources.Requests = corev1.ResourceList{}
		}
		for name, limit := range c.Resources.Limits {
			if _, set := c.Resources.Requests[name]; !set {
				c.Resources.Requests[name] = limit.DeepCopy()
			}
		}
	}
	return pod
}

func validateRuntimeTrustBundle(ctx context.Context, c client.Client, rt *sprooziv1alpha1.AgentRuntime) error {
	reference := rt.Spec.ClientConfig.TrustBundleConfigMap
	if reference.Name == "" || reference.Key == "" {
		return fmt.Errorf("runtime client trust bundle reference is required")
	}
	var configMap corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: AgentsNamespace, Name: reference.Name}, &configMap); err != nil {
		return fmt.Errorf("get runtime trust bundle: %w", err)
	}
	if configMap.Immutable == nil || !*configMap.Immutable {
		return fmt.Errorf("runtime trust bundle ConfigMap must be immutable")
	}
	bundle, ok := configMap.Data[reference.Key]
	if !ok || len(bundle) == 0 || len(bundle) > maxTrustBundleBytes {
		return fmt.Errorf("runtime trust bundle is missing or exceeds %d bytes", maxTrustBundleBytes)
	}
	remaining := []byte(bundle)
	certificates := 0
	for len(bytes.TrimSpace(remaining)) > 0 {
		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return fmt.Errorf("runtime trust bundle must contain only PEM certificates")
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return fmt.Errorf("runtime trust bundle contains an invalid certificate: %w", err)
		}
		certificates++
		remaining = rest
	}
	if certificates == 0 {
		return fmt.Errorf("runtime trust bundle must contain at least one certificate")
	}
	return nil
}

func ptrTo(value bool) *bool { return &value }

func tokenVolumeSource(audience string, defaultMode int32) corev1.VolumeSource {
	return corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{
		DefaultMode: &defaultMode,
		Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
			Audience: audience, ExpirationSeconds: ptrToInt64(gatewayTokenExpirationSeconds), Path: "token",
		}}},
	}}
}

func ptrToInt64(value int64) *int64 { return &value }

func requestsCapability(run *sprooziv1alpha1.AgentRun, capability sprooziv1alpha1.CapabilityKind) bool {
	return slices.Contains(run.Spec.Capabilities, capability)
}
