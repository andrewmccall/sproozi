package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/agentcontract"
	"github.com/andrewmccall/sproozi/internal/harness"
)

const (
	clientAuthPlaceholder  = "sproozi-local-placeholder"
	kubeconfigContextName  = "sproozi"
	contractVolumeName     = "contract"
	gatewayTokenVolumeName = "gateway-token"
	appsAPIGroup           = "apps"
)

const (
	contractConfigMapKey   = "input.json"
	kubeconfigConfigMapKey = "kubeconfig"
)

// BuildAgentContract constructs the single strict runner input for an admitted run.
func BuildAgentContract(run *sprooziv1alpha1.AgentRun, tmpl *sprooziv1alpha1.AgentTemplate, rt *sprooziv1alpha1.AgentRuntime) (agentcontract.Input, error) {
	eventContext, err := json.Marshal(run.Spec.EventContext)
	if err != nil {
		return agentcontract.Input{}, fmt.Errorf("encode event context: %w", err)
	}
	if bytes.Equal(eventContext, []byte("null")) {
		eventContext = []byte("{}")
	}
	input := agentcontract.Input{
		Version:      agentcontract.Version,
		Run:          agentcontract.RunIdentity{Namespace: run.Namespace, Name: run.Name, UID: string(run.UID)},
		Trusted:      agentcontract.TrustedInput{Instructions: tmpl.Spec.Instructions},
		Untrusted:    agentcontract.UntrustedInput{Task: run.Spec.Task, EventContext: eventContext},
		Capabilities: capabilityNames(run.Spec.Capabilities),
		Workspace:    "/workspace",
		Gateway:      agentcontract.GatewayBinding{Endpoint: rt.Spec.GatewayEndpoint, TokenPath: gatewayTokenPath},
	}
	if err := input.Validate(); err != nil {
		return agentcontract.Input{}, err
	}
	return input, nil
}

// EnsureAgentContract writes an immutable ConfigMap before the agent Pod exists.
// A pre-existing differing document is an identity collision and fails closed.
func EnsureAgentContract(ctx context.Context, c client.Client, run *sprooziv1alpha1.AgentRun, tmpl *sprooziv1alpha1.AgentTemplate, rt *sprooziv1alpha1.AgentRuntime) error {
	input, err := BuildAgentContract(run, tmpl, rt)
	if err != nil {
		return err
	}
	document, err := json.Marshal(input)
	if err != nil {
		return fmt.Errorf("encode agent contract: %w", err)
	}
	wanted := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: RunName(run.UID), Namespace: AgentsNamespace, Labels: map[string]string{
			RunLabel: run.Name, RunNamespaceLabel: run.Namespace, RunUIDLabel: string(run.UID), ManagedByLabel: ManagedByValue,
		}},
		Immutable: ptrTo(true),
		Data: map[string]string{
			contractConfigMapKey:   string(document),
			kubeconfigConfigMapKey: generatedKubeconfig(),
		},
	}
	config, err := harness.MCPConfig(rt.Spec.ClientConfig.Harness, input.Capabilities, rt.Spec.GatewayEndpoint)
	if err != nil {
		return err
	}
	maps.Copy(wanted.Data, config)
	if rt.Spec.ClientConfig.Harness == "opencode" {
		wanted.Data["opencode-launch.py"] = harness.OpenCodeLaunch
	}
	if rt.Spec.ClientConfig.Harness == "claude-code" || rt.Spec.ClientConfig.Harness == "opencode" {
		request, err := json.Marshal(input.Untrusted)
		if err != nil {
			return fmt.Errorf("encode untrusted request: %w", err)
		}
		wanted.Data["instructions.txt"] = input.Trusted.Instructions
		wanted.Data["request.json"] = string(request)
	}
	if err := c.Create(ctx, wanted); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return err
	}
	var existing corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKeyFromObject(wanted), &existing); err != nil {
		return err
	}
	if existing.Immutable == nil || !*existing.Immutable || existing.Labels[RunUIDLabel] != string(run.UID) || !maps.Equal(existing.Data, wanted.Data) || len(existing.BinaryData) != 0 {
		return fmt.Errorf("existing agent contract %s has different immutable contents", wanted.Name)
	}
	return nil
}

func generatedKubeconfig() string {
	config := clientcmdapi.Config{
		Clusters: map[string]*clientcmdapi.Cluster{kubeconfigContextName: {
			Server: "https://kubernetes.default.svc",
			// The gateway terminates the inspected TLS session. The workload
			// trusts only the administrator-owned gateway CA and never receives a
			// Kubernetes API credential or direct API-server route.
			CertificateAuthority: trustBundlePath,
		}},
		// A non-secret placeholder prevents kubectl's interactive credential
		// prompt. Only the outer proxy's projected Run token authenticates work;
		// the shared transport strips this inner Authorization header.
		AuthInfos: map[string]*clientcmdapi.AuthInfo{kubeconfigContextName: {Token: clientAuthPlaceholder}},
		Contexts: map[string]*clientcmdapi.Context{kubeconfigContextName: {
			Cluster:  kubeconfigContextName,
			AuthInfo: kubeconfigContextName,
		}},
		CurrentContext: kubeconfigContextName,
	}
	document, err := clientcmd.Write(config)
	if err != nil {
		panic(fmt.Sprintf("encode static kubeconfig: %v", err))
	}
	return string(document)
}

// RevokeAgentContract removes the run-owned contract after its Pod has stopped.
func RevokeAgentContract(ctx context.Context, c client.Client, run *sprooziv1alpha1.AgentRun, expectedUID ...string) error {
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: RunName(run.UID), Namespace: AgentsNamespace}}
	var existing corev1.ConfigMap
	if err := c.Get(ctx, client.ObjectKeyFromObject(configMap), &existing); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if len(expectedUID) > 0 && !ownedByRun(&existing, expectedUID[0]) {
		return nil
	}
	if err := c.Delete(ctx, &existing, client.Preconditions{UID: &existing.UID, ResourceVersion: &existing.ResourceVersion}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

func capabilityNames(capabilities []sprooziv1alpha1.CapabilityKind) []string {
	result := make([]string, len(capabilities))
	for i, capability := range capabilities {
		result[i] = string(capability)
	}
	return result
}
