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
package kubernetes_test

import (
	"context"
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/kubernetes"
)

func TestEnsureNetworkPolicyCreates(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	run := testRun("bbbbbbbb-0000-0000-0000-000000000001")

	if err := kubernetes.EnsureNetworkPolicy(context.Background(), c, run); err != nil {
		t.Fatalf("EnsureNetworkPolicy error: %v", err)
	}

	npName := kubernetes.RunName(run.UID)
	var np networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: kubernetes.AgentsNamespace, Name: npName}, &np); err != nil {
		t.Fatalf("NetworkPolicy not found: %v", err)
	}
	if np.Spec.PodSelector.MatchLabels[kubernetes.RunLabel] != run.Name {
		t.Errorf("NetworkPolicy pod selector does not target run: %+v", np.Spec.PodSelector)
	}
}

func TestNamedMCPGrantUsesOnlyExistingGatewayNetworkRoute(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	run := testRun("bbbbbbbb-0000-0000-0000-000000000301")
	run.Spec.Capabilities = []sprooziv1alpha1.CapabilityKind{testMCPCapability}
	p := &sprooziv1alpha1.AgentPolicy{Spec: sprooziv1alpha1.AgentPolicySpec{AllowedCapabilities: []sprooziv1alpha1.CapabilityKind{testMCPCapability}}}
	if err := kubernetes.EnsureNetworkPolicy(t.Context(), c, run, p); err != nil {
		t.Fatal(err)
	}
	var np networkingv1.NetworkPolicy
	key := client.ObjectKey{Namespace: kubernetes.AgentsNamespace, Name: kubernetes.RunName(run.UID)}
	if err := c.Get(t.Context(), key, &np); err != nil {
		t.Fatal(err)
	}
	if len(np.Spec.Egress) != 2 || np.Spec.Egress[1].Ports[0].Port.IntVal != 8443 || np.Spec.Egress[1].To[0].PodSelector == nil {
		t.Fatalf("MCP network route=%+v", np.Spec.Egress)
	}
	p.Spec.AllowedCapabilities = nil
	if err := kubernetes.EnsureNetworkPolicy(t.Context(), c, run, p); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(t.Context(), key, &np); err != nil {
		t.Fatal(err)
	}
	if len(np.Spec.Egress) != 1 {
		t.Fatal("revoked MCP grant retained gateway route")
	}
}

func TestNetworkPolicyDeniesAllIngress(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	run := testRun("bbbbbbbb-0000-0000-0000-000000000002")
	_ = kubernetes.EnsureNetworkPolicy(context.Background(), c, run)
	npName := kubernetes.RunName(run.UID)
	var np networkingv1.NetworkPolicy
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: kubernetes.AgentsNamespace, Name: npName}, &np)

	hasIngress := false
	for _, pt := range np.Spec.PolicyTypes {
		if pt == networkingv1.PolicyTypeIngress {
			hasIngress = true
		}
	}
	if !hasIngress {
		t.Error("NetworkPolicy does not declare Ingress policy type")
	}
	if len(np.Spec.Ingress) != 0 {
		t.Errorf("NetworkPolicy has ingress rules (should deny all): %+v", np.Spec.Ingress)
	}
}

func TestNetworkPolicyAllowsDNS(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	run := testRun("bbbbbbbb-0000-0000-0000-000000000003")
	_ = kubernetes.EnsureNetworkPolicy(context.Background(), c, run)
	npName := kubernetes.RunName(run.UID)
	var np networkingv1.NetworkPolicy
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: kubernetes.AgentsNamespace, Name: npName}, &np)

	found53UDP := false
	for _, rule := range np.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == 53 && port.Protocol != nil && *port.Protocol == corev1.ProtocolUDP {
				found53UDP = true
			}
		}
	}
	if !found53UDP {
		t.Error("NetworkPolicy missing DNS (UDP 53) egress rule")
	}
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.NamespaceSelector != nil && peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == "kube-system" {
				if peer.PodSelector == nil || peer.PodSelector.MatchLabels["k8s-app"] != "kube-dns" {
					t.Error("DNS egress must select the kube-dns Pods, not the whole namespace")
				}
			}
		}
	}
}

func TestNetworkPolicyAllowsSproozSystem(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	run := testRun("bbbbbbbb-0000-0000-0000-000000000004")
	run.Spec.Capabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityModelInference, sprooziv1alpha1.CapabilityGitHubPullRequest, sprooziv1alpha1.CapabilityNetworkEgress}
	_ = kubernetes.EnsureNetworkPolicy(context.Background(), c, run)
	npName := kubernetes.RunName(run.UID)
	var np networkingv1.NetworkPolicy
	_ = c.Get(context.Background(), client.ObjectKey{Namespace: kubernetes.AgentsNamespace, Name: npName}, &np)

	foundSproozi := false
	foundPorts := map[int32]bool{8443: false}
	for _, rule := range np.Spec.Egress {
		for _, peer := range rule.To {
			if peer.NamespaceSelector != nil {
				if v, ok := peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"]; ok && v == "sproozi-system" {
					foundSproozi = true
					if peer.PodSelector == nil || peer.PodSelector.MatchLabels["app.kubernetes.io/component"] != "shared-gateway" {
						t.Error("gateway egress must select labelled gateway Pods, not the whole namespace")
					}
					for _, port := range rule.Ports {
						if port.Port != nil {
							if _, ok := foundPorts[port.Port.IntVal]; ok && port.Protocol != nil && *port.Protocol == corev1.ProtocolTCP {
								foundPorts[port.Port.IntVal] = true
							}
						}
					}
				}
			}
		}
	}
	if !foundSproozi {
		t.Error("NetworkPolicy missing egress rule for sproozi-system namespace")
	}
	for port, found := range foundPorts {
		if !found {
			t.Errorf("NetworkPolicy missing TCP %d egress rule for sproozi-system namespace", port)
		}
	}
}

func TestNetworkPolicyRemovesGatewayRoutesWithoutCapability(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	run := testRun("bbbbbbbb-0000-0000-0000-000000000007")
	run.Spec.Capabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityModelInference}
	if err := kubernetes.EnsureNetworkPolicy(context.Background(), c, run); err != nil {
		t.Fatal(err)
	}
	var np networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: kubernetes.AgentsNamespace, Name: kubernetes.RunName(run.UID)}, &np); err != nil {
		t.Fatal(err)
	}
	ports := map[int32]bool{}
	for _, rule := range np.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil {
				ports[port.Port.IntVal] = true
			}
		}
	}
	if !ports[8443] || ports[8080] || ports[8081] || ports[8082] || ports[443] {
		t.Fatalf("gateway routes = %#v, want only shared gateway route", ports)
	}
}

func TestNetworkPolicyAddsPackageRouteWithoutRawEgress(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	run := testRun("bbbbbbbb-0000-0000-0000-000000000009")
	run.Spec.Capabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityPackagesInstall}
	if err := kubernetes.EnsureNetworkPolicy(context.Background(), c, run); err != nil {
		t.Fatal(err)
	}
	var np networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: kubernetes.AgentsNamespace, Name: kubernetes.RunName(run.UID)}, &np); err != nil {
		t.Fatal(err)
	}
	ports := map[int32]bool{}
	for _, rule := range np.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil {
				ports[port.Port.IntVal] = true
			}
		}
	}
	if !ports[53] || !ports[8443] || ports[8080] || ports[8081] || ports[8082] || ports[443] {
		t.Fatalf("package-only routes = %#v, want DNS and shared gateway only", ports)
	}
}

func TestNetworkPolicyRemovesRoutesWhenLivePolicyTightens(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	run := testRun("bbbbbbbb-0000-0000-0000-000000000010")
	run.Spec.Capabilities = []sprooziv1alpha1.CapabilityKind{
		sprooziv1alpha1.CapabilityModelInference,
		sprooziv1alpha1.CapabilityGitHubPullRequest,
		sprooziv1alpha1.CapabilityNetworkEgress,
	}
	policy := &sprooziv1alpha1.AgentPolicy{}
	policy.Spec.AllowedCapabilities = slices.Clone(run.Spec.Capabilities)
	if err := kubernetes.EnsureNetworkPolicy(context.Background(), c, run, policy); err != nil {
		t.Fatal(err)
	}
	policy.Spec.AllowedCapabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityModelInference}
	if err := kubernetes.EnsureNetworkPolicy(context.Background(), c, run, policy); err != nil {
		t.Fatal(err)
	}
	var np networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: kubernetes.AgentsNamespace, Name: kubernetes.RunName(run.UID)}, &np); err != nil {
		t.Fatal(err)
	}
	for _, rule := range np.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil && (port.Port.IntVal == 8081 || port.Port.IntVal == 8082) {
				t.Fatalf("revoked policy route remained on port %d", port.Port.IntVal)
			}
		}
	}
}

func TestEnsureNetworkPolicyIdempotent(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	run := testRun("bbbbbbbb-0000-0000-0000-000000000005")
	if err := kubernetes.EnsureNetworkPolicy(context.Background(), c, run); err != nil {
		t.Fatalf("first EnsureNetworkPolicy error: %v", err)
	}
	if err := kubernetes.EnsureNetworkPolicy(context.Background(), c, run); err != nil {
		t.Fatalf("second EnsureNetworkPolicy error: %v", err)
	}
}

func TestEnsureNetworkPolicyTightensExistingRoutes(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	run := testRun("bbbbbbbb-0000-0000-0000-000000000008")
	run.Spec.Capabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityModelInference, sprooziv1alpha1.CapabilityNetworkEgress}
	if err := kubernetes.EnsureNetworkPolicy(context.Background(), c, run); err != nil {
		t.Fatal(err)
	}
	run.Spec.Capabilities = []sprooziv1alpha1.CapabilityKind{sprooziv1alpha1.CapabilityModelInference}
	if err := kubernetes.EnsureNetworkPolicy(context.Background(), c, run); err != nil {
		t.Fatal(err)
	}
	var np networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: kubernetes.AgentsNamespace, Name: kubernetes.RunName(run.UID)}, &np); err != nil {
		t.Fatal(err)
	}
	for _, rule := range np.Spec.Egress {
		for _, port := range rule.Ports {
			if port.Port != nil && port.Port.IntVal == 8082 {
				t.Fatal("revoked network.egress route remained")
			}
		}
	}
}

func TestRevokeNetworkPolicyDeletes(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	run := testRun("bbbbbbbb-0000-0000-0000-000000000006")
	_ = kubernetes.EnsureNetworkPolicy(context.Background(), c, run)
	npName := kubernetes.RunName(run.UID)
	if err := kubernetes.RevokeNetworkPolicy(context.Background(), c, npName); err != nil {
		t.Fatalf("RevokeNetworkPolicy error: %v", err)
	}
	var np networkingv1.NetworkPolicy
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: kubernetes.AgentsNamespace, Name: npName}, &np); err == nil {
		t.Error("NetworkPolicy still exists after revoke")
	}
}

func TestRevokeNetworkPolicyNotFoundIsOK(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	if err := kubernetes.RevokeNetworkPolicy(context.Background(), c, "sproozi-nonexistent"); err != nil {
		t.Errorf("RevokeNetworkPolicy on missing NP should not error: %v", err)
	}
}
