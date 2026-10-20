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
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/agentcontract"
)

const (
	agentContainerName  = "agent"
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
	// SandboxStatusAbsent means a successful read established that the Pod is missing.
	SandboxStatusAbsent SandboxStatus = "Absent"
	// SandboxStatusUnknown means an existing Pod has an uncertain lifecycle phase.
	SandboxStatusUnknown SandboxStatus = "Unknown"
)

// ErrInvalidRuntime marks an AgentRuntime that cannot launch a sandbox until an
// operator fixes it. Other ValidateRuntime errors are transient.
var ErrInvalidRuntime = errors.New("cannot launch a sandbox")

// SandboxClient abstracts Sandbox Pod lifecycle operations for unit-testable reconciliation.
type SandboxClient interface {
	// ValidateRuntime checks that rt can launch a sandbox. Run admission calls
	// it so an unlaunchable runtime fails the run before it takes the active
	// slot. Errors wrapping ErrInvalidRuntime are permanent.
	ValidateRuntime(ctx context.Context, rt *sprooziv1alpha1.AgentRuntime) error

	// Ensure creates the Sandbox Pod if it does not already exist. Idempotent.
	// rt must already have passed ValidateRuntime.
	// Returns the sandbox name (always RunName(run.UID)).
	Ensure(ctx context.Context, run *sprooziv1alpha1.AgentRun, rt *sprooziv1alpha1.AgentRuntime) (string, error)

	// GetStatus returns the current lifecycle status of the named Sandbox.
	// Returns SandboxStatusAbsent if the Pod does not exist.
	GetStatus(ctx context.Context, name string) (SandboxStatus, error)

	// GetCompletion observes actual exit and optional untrusted result from one
	// UID-owned Pod read. Only valid when the agent container has terminated.
	GetCompletion(ctx context.Context, name, runUID string) (SandboxCompletion, error)

	// DeleteForRun removes the named Sandbox only while it still belongs to
	// runUID. NotFound is success.
	DeleteForRun(ctx context.Context, name, runUID string) error
}

// SandboxCompletion is one terminal container observation. Result may be nil
// even when ExitCode is zero; output availability is independent of success.
type SandboxCompletion struct {
	ExitCode int32
	Result   *sprooziv1alpha1.AgentRunResult
}

// IsQuotaExceeded reports whether err is a namespace ResourceQuota rejecting
// a create because the quota is used up. Such a rejection can clear when other
// workloads finish, so callers retry rather than fail.
func IsQuotaExceeded(err error) bool {
	return err != nil && apierrors.IsForbidden(err) && strings.Contains(err.Error(), "exceeded quota")
}

// PodSandboxClient implements SandboxClient using a Kubernetes Pod.
type PodSandboxClient struct {
	c client.Client
	// live reads the API server directly. Absent ends a run, so GetStatus
	// confirms a cached NotFound with it: the cache can lag a Pod just created.
	live client.Reader
}

// NewPodSandboxClient returns the Pod-backed execution adapter. live must read
// the API server directly, not through the cache that backs c.
func NewPodSandboxClient(c client.Client, live client.Reader) *PodSandboxClient {
	return &PodSandboxClient{c: c, live: live}
}

// ValidateRuntime checks the Pod template and the trust bundle ConfigMap it mounts.
func (p *PodSandboxClient) ValidateRuntime(ctx context.Context, rt *sprooziv1alpha1.AgentRuntime) error {
	if err := rt.Spec.ValidateRuntimeTemplate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRuntime, err)
	}
	return validateRuntimeTrustBundle(ctx, p.c, rt)
}

// Ensure creates the Sandbox Pod if it does not already exist. Idempotent.
func (p *PodSandboxClient) Ensure(ctx context.Context, run *sprooziv1alpha1.AgentRun, rt *sprooziv1alpha1.AgentRuntime) (string, error) {
	saName := run.Status.Identity.ServiceAccountName
	pod := BuildPodSpec(run, rt, saName)
	return p.ensurePod(ctx, pod)
}

func (p *PodSandboxClient) ensurePod(ctx context.Context, pod *corev1.Pod) (string, error) {
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
	if slices.Contains(wanted.Finalizers, ManagedWriterFenceFinalizer) && !slices.Contains(existing.Finalizers, ManagedWriterFenceFinalizer) {
		return fmt.Errorf("existing managed sandbox %s lacks writer fence retention", wanted.Name)
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
	// The API server fills unset fields with defaults, so compare both specs
	// with the same defaults applied. Defaulting only fills unset fields: any
	// value set on the existing Pod is still compared. Semantic equality
	// compares Quantities by value: their cached string form depends on how and
	// when each copy was decoded or printed.
	e, w := *withAPIDefaults(&existing.Spec), *withAPIDefaults(&wanted.Spec)
	var differs []string
	for field, same := range map[string]bool{
		"serviceAccountName":           e.ServiceAccountName == w.ServiceAccountName,
		"automountServiceAccountToken": e.AutomountServiceAccountToken != nil && reflect.DeepEqual(e.AutomountServiceAccountToken, w.AutomountServiceAccountToken),
		"hostNamespaces":               e.HostNetwork == w.HostNetwork && e.HostPID == w.HostPID && e.HostIPC == w.HostIPC,
		"shareProcessNamespace":        e.ShareProcessNamespace == nil || !*e.ShareProcessNamespace,
		"restartPolicy":                e.RestartPolicy == w.RestartPolicy,
		"securityContext":              reflect.DeepEqual(e.SecurityContext, w.SecurityContext),
		"runtimeClassName":             reflect.DeepEqual(e.RuntimeClassName, w.RuntimeClassName),
		"containers":                   apiequality.Semantic.DeepEqual(e.Containers, w.Containers),
		"initContainers":               apiequality.Semantic.DeepEqual(e.InitContainers, w.InitContainers),
		"ephemeralContainers":          apiequality.Semantic.DeepEqual(e.EphemeralContainers, w.EphemeralContainers),
		"volumes":                      apiequality.Semantic.DeepEqual(e.Volumes, w.Volumes),
	} {
		if !same {
			differs = append(differs, field)
		}
	}
	if len(differs) > 0 {
		slices.Sort(differs)
		return fmt.Errorf("existing sandbox %s has unsafe Pod settings: %s", wanted.Name, strings.Join(differs, ", "))
	}
	return nil
}

// withAPIDefaults returns a copy of spec with the defaults the API server
// applies to the fields validateExistingSandbox compares. A runtime template may
// leave these unset, for example a probe's thresholds or a ConfigMap volume's
// mode, and the stored Pod then differs from the built one only by defaults.
func withAPIDefaults(spec *corev1.PodSpec) *corev1.PodSpec {
	out := spec.DeepCopy()
	for i := range out.Containers {
		defaultContainer(&out.Containers[i])
	}
	for i := range out.InitContainers {
		defaultContainer(&out.InitContainers[i])
	}
	for i := range out.Volumes {
		defaultVolume(&out.Volumes[i].VolumeSource)
	}
	return out
}

func defaultContainer(c *corev1.Container) {
	if c.ImagePullPolicy == "" {
		c.ImagePullPolicy = corev1.PullIfNotPresent
		if !strings.Contains(c.Image, "@") {
			if _, tag, found := strings.Cut(c.Image[strings.LastIndex(c.Image, "/")+1:], ":"); !found || tag == "latest" {
				c.ImagePullPolicy = corev1.PullAlways
			}
		}
	}
	if c.TerminationMessagePath == "" {
		c.TerminationMessagePath = corev1.TerminationMessagePathDefault
	}
	if c.TerminationMessagePolicy == "" {
		c.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}
	for i := range c.Ports {
		if c.Ports[i].Protocol == "" {
			c.Ports[i].Protocol = corev1.ProtocolTCP
		}
	}
	for i := range c.Env {
		if from := c.Env[i].ValueFrom; from != nil {
			defaultFieldRef(from.FieldRef)
		}
	}
	for _, probe := range []*corev1.Probe{c.LivenessProbe, c.ReadinessProbe, c.StartupProbe} {
		if probe == nil {
			continue
		}
		defaultHTTPGet(probe.HTTPGet)
		if probe.TimeoutSeconds == 0 {
			probe.TimeoutSeconds = 1
		}
		if probe.PeriodSeconds == 0 {
			probe.PeriodSeconds = 10
		}
		if probe.SuccessThreshold == 0 {
			probe.SuccessThreshold = 1
		}
		if probe.FailureThreshold == 0 {
			probe.FailureThreshold = 3
		}
	}
	if c.Lifecycle != nil {
		for _, handler := range []*corev1.LifecycleHandler{c.Lifecycle.PostStart, c.Lifecycle.PreStop} {
			if handler != nil {
				defaultHTTPGet(handler.HTTPGet)
			}
		}
	}
}

func defaultHTTPGet(action *corev1.HTTPGetAction) {
	if action == nil {
		return
	}
	if action.Path == "" {
		action.Path = "/"
	}
	if action.Scheme == "" {
		action.Scheme = corev1.URISchemeHTTP
	}
}

func defaultFieldRef(ref *corev1.ObjectFieldSelector) {
	if ref != nil && ref.APIVersion == "" {
		ref.APIVersion = "v1"
	}
}

func defaultVolume(source *corev1.VolumeSource) {
	mode := corev1.ConfigMapVolumeSourceDefaultMode
	switch {
	case source.ConfigMap != nil && source.ConfigMap.DefaultMode == nil:
		source.ConfigMap.DefaultMode = &mode
	case source.Secret != nil && source.Secret.DefaultMode == nil:
		source.Secret.DefaultMode = &mode
	case source.DownwardAPI != nil:
		if source.DownwardAPI.DefaultMode == nil {
			source.DownwardAPI.DefaultMode = &mode
		}
		for i := range source.DownwardAPI.Items {
			defaultFieldRef(source.DownwardAPI.Items[i].FieldRef)
		}
	case source.Projected != nil:
		if source.Projected.DefaultMode == nil {
			source.Projected.DefaultMode = &mode
		}
		for i := range source.Projected.Sources {
			projection := &source.Projected.Sources[i]
			if token := projection.ServiceAccountToken; token != nil && token.ExpirationSeconds == nil {
				expiration := int64(3600)
				token.ExpirationSeconds = &expiration
			}
			if projection.DownwardAPI != nil {
				for j := range projection.DownwardAPI.Items {
					defaultFieldRef(projection.DownwardAPI.Items[j].FieldRef)
				}
			}
		}
	}
}

// GetStatus returns the current lifecycle status of the named Sandbox.
func (p *PodSandboxClient) GetStatus(ctx context.Context, name string) (SandboxStatus, error) {
	var pod corev1.Pod
	key := client.ObjectKey{Namespace: AgentsNamespace, Name: name}
	err := p.c.Get(ctx, key, &pod)
	if apierrors.IsNotFound(err) {
		err = p.live.Get(ctx, key, &pod)
	}
	if err != nil {
		if apierrors.IsNotFound(err) {
			return SandboxStatusAbsent, nil
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

// GetCompletion reads exit and bounded result from the same owned Pod incarnation.
func (p *PodSandboxClient) GetCompletion(ctx context.Context, name, runUID string) (SandboxCompletion, error) {
	var pod corev1.Pod
	if err := p.c.Get(ctx, client.ObjectKey{Namespace: AgentsNamespace, Name: name}, &pod); err != nil {
		return SandboxCompletion{}, err
	}
	if runUID == "" || !ownedByRun(&pod, runUID) {
		return SandboxCompletion{}, fmt.Errorf("sandbox does not belong to the expected run")
	}
	// FallbackToLogsOnError may expose logs rather than explicit publication.
	// Only the File policy establishes this result channel.
	resultFromFile := false
	for _, container := range pod.Spec.Containers {
		if container.Name == agentContainerName {
			resultFromFile = container.TerminationMessagePolicy == corev1.TerminationMessageReadFile
		}
	}

	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == agentContainerName && cs.State.Terminated != nil {
			completion := SandboxCompletion{ExitCode: cs.State.Terminated.ExitCode}
			if !resultFromFile {
				return completion, nil
			}
			result, err := agentcontract.DecodeResult([]byte(cs.State.Terminated.Message), runUID)
			if err == nil {
				completion.Result = &sprooziv1alpha1.AgentRunResult{Text: result.Text, Truncated: result.Truncated}
			}
			return completion, nil
		}
	}
	return SandboxCompletion{}, fmt.Errorf("agent container has not terminated")
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
				Name: workspaceVolume,
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
		workload = []string{agentContainerName}
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
			corev1.EnvVar{Name: openAIAPIKey, Value: clientAuthPlaceholder},
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
			corev1.VolumeMount{Name: workspaceVolume, MountPath: workspaceDirectory}, corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"},
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
		return fmt.Errorf("%w: runtime client trust bundle reference is required", ErrInvalidRuntime)
	}
	var configMap corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKey{Namespace: AgentsNamespace, Name: reference.Name}, &configMap); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: runtime trust bundle ConfigMap %s/%s not found", ErrInvalidRuntime, AgentsNamespace, reference.Name)
		}
		return fmt.Errorf("get runtime trust bundle: %w", err)
	}
	if configMap.Immutable == nil || !*configMap.Immutable {
		return fmt.Errorf("%w: runtime trust bundle ConfigMap must be immutable", ErrInvalidRuntime)
	}
	bundle, ok := configMap.Data[reference.Key]
	if !ok || len(bundle) == 0 || len(bundle) > maxTrustBundleBytes {
		return fmt.Errorf("%w: runtime trust bundle is missing or exceeds %d bytes", ErrInvalidRuntime, maxTrustBundleBytes)
	}
	remaining := []byte(bundle)
	certificates := 0
	for len(bytes.TrimSpace(remaining)) > 0 {
		block, rest := pem.Decode(remaining)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return fmt.Errorf("%w: runtime trust bundle must contain only PEM certificates", ErrInvalidRuntime)
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return fmt.Errorf("%w: runtime trust bundle contains an invalid certificate: %w", ErrInvalidRuntime, err)
		}
		certificates++
		remaining = rest
	}
	if certificates == 0 {
		return fmt.Errorf("%w: runtime trust bundle must contain at least one certificate", ErrInvalidRuntime)
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
