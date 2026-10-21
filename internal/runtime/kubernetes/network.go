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
	"context"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

var (
	protocolTCP     = corev1.ProtocolTCP
	port8443        = intstr.FromInt32(8443)
	gatewayPodLabel = "app.kubernetes.io/component"
)

// namespaceNameLabel is the standard Kubernetes label used to select namespaces by name.
const namespaceNameLabel = "kubernetes.io/metadata.name"

// EnsureNetworkPolicy creates a default-deny NetworkPolicy in AgentsNamespace
// for the run's sandbox pods. The only allowed egress is the shared Sproozi
// gateway (8443/TCP) in sproozi-system, and only while the run holds an
// allowed capability. DNS is not allowed; see PinGateway.
//
// Ingress is denied entirely (empty ingress rules list).
// Pod selector targets pods with RunLabel=run.Name.
// Idempotent. When a live policy is supplied, routes are compiled from the
// intersection of immutable run capabilities and currently allowed policy
// capabilities, so policy tightening removes network reachability during the
// same reconciliation that revokes the gateway operation.
func EnsureNetworkPolicy(ctx context.Context, c client.Client, run *sprooziv1alpha1.AgentRun, policies ...*sprooziv1alpha1.AgentPolicy) error {
	npName := RunName(run.UID)
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      npName,
			Namespace: AgentsNamespace,
			Labels: map[string]string{
				RunLabel:       run.Name,
				RunUIDLabel:    string(run.UID),
				ManagedByLabel: ManagedByValue,
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			// Select by UID: run names repeat across namespaces, and policies
			// are additive, so a name selector would share egress between runs.
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{RunUIDLabel: string(run.UID)},
			},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{}, // deny all
		},
	}
	var policy *sprooziv1alpha1.AgentPolicy
	if len(policies) > 0 {
		policy = policies[0]
	}
	np.Spec.Egress = permittedEgress(run, policy)
	if err := c.Create(ctx, np); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return err
	}
	var existing networkingv1.NetworkPolicy
	if err := c.Get(ctx, client.ObjectKeyFromObject(np), &existing); err != nil {
		return err
	}
	if existing.Labels[ManagedByLabel] != ManagedByValue || existing.Labels[RunLabel] != run.Name || existing.Labels[RunUIDLabel] != string(run.UID) {
		return fmt.Errorf("existing NetworkPolicy %s has different run identity", np.Name)
	}
	existing.Spec = np.Spec
	return c.Update(ctx, &existing)
}

// permittedEgress allows only the shared gateway, and only when the run has a
// capability the policy still allows. There is no DNS rule: the sandbox
// reaches the gateway by the address pinned in its hosts file (PinGateway).
func permittedEgress(run *sprooziv1alpha1.AgentRun, policy *sprooziv1alpha1.AgentPolicy) []networkingv1.NetworkPolicyEgressRule {
	rules := []networkingv1.NetworkPolicyEgressRule{}
	for _, capability := range run.Spec.Capabilities {
		if capabilityAllowed(run, policy, capability) {
			rules = append(rules, gatewayRule("sproozi-system", "shared-gateway", port8443))
			break
		}
	}
	return rules
}

func capabilityAllowed(run *sprooziv1alpha1.AgentRun, policy *sprooziv1alpha1.AgentPolicy, capability sprooziv1alpha1.CapabilityKind) bool {
	if !requestsCapability(run, capability) {
		return false
	}
	return policy == nil || slices.Contains(policy.Spec.AllowedCapabilities, capability)
}

func gatewayRule(namespace, component string, port intstr.IntOrString) networkingv1.NetworkPolicyEgressRule {
	peer := networkingv1.NetworkPolicyPeer{NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{namespaceNameLabel: namespace}}}
	if component != "" {
		peer.PodSelector = &metav1.LabelSelector{MatchLabels: map[string]string{gatewayPodLabel: component}}
	}
	return networkingv1.NetworkPolicyEgressRule{Ports: []networkingv1.NetworkPolicyPort{{Protocol: &protocolTCP, Port: &port}}, To: []networkingv1.NetworkPolicyPeer{peer}}
}

// RevokeNetworkPolicy deletes the named NetworkPolicy from AgentsNamespace.
// Not-found is treated as success.
func RevokeNetworkPolicy(ctx context.Context, c client.Client, npName string, expectedUID ...string) error {
	np := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      npName,
			Namespace: AgentsNamespace,
		},
	}
	var existing networkingv1.NetworkPolicy
	if err := c.Get(ctx, client.ObjectKeyFromObject(np), &existing); err != nil {
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
