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
	"testing"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/kubernetes"
)

func TestKubernetesReadRulesCompilesApprovedResourcesOnly(t *testing.T) {
	rules, err := kubernetes.KubernetesReadRules([]sprooziv1alpha1.KubernetesReadResource{
		sprooziv1alpha1.KubernetesReadPods,
		sprooziv1alpha1.KubernetesReadPodLogs,
		sprooziv1alpha1.KubernetesReadEvents,
		sprooziv1alpha1.KubernetesReadServices,
		sprooziv1alpha1.KubernetesReadDeployments,
		sprooziv1alpha1.KubernetesReadReplicaSets,
		sprooziv1alpha1.KubernetesReadStatefulSets,
	})
	if err != nil {
		t.Fatal(err)
	}
	resources := map[string]bool{}
	for _, rule := range rules {
		if len(rule.Verbs) != 3 || rule.Verbs[0] != "get" || rule.Verbs[1] != "list" || rule.Verbs[2] != "watch" {
			t.Fatalf("unexpected verbs: %#v", rule.Verbs)
		}
		for _, resource := range rule.Resources {
			resources[resource] = true
		}
	}
	for _, resource := range []string{"pods", "pods/log", "events", "services", "deployments", "replicasets", "statefulsets"} {
		if !resources[resource] {
			t.Errorf("missing approved resource %q", resource)
		}
	}
	for _, resource := range []string{"secrets", "configmaps", "pods/exec", "pods/attach", "pods/portforward"} {
		if resources[resource] {
			t.Errorf("forbidden resource %q was compiled", resource)
		}
	}
}

func TestKubernetesReadRulesRejectsUnsafeOrDuplicateResources(t *testing.T) {
	for _, resources := range [][]sprooziv1alpha1.KubernetesReadResource{
		nil,
		{sprooziv1alpha1.KubernetesReadPods, sprooziv1alpha1.KubernetesReadPods},
		{sprooziv1alpha1.KubernetesReadResource("secrets")},
		{sprooziv1alpha1.KubernetesReadResource("*")},
	} {
		if _, err := kubernetes.KubernetesReadRules(resources); err == nil {
			t.Fatalf("expected scope %v to be rejected", resources)
		}
	}
}

func TestKubernetesReadRulesNeverEmitWildcardOrWrite(t *testing.T) {
	rules, err := kubernetes.KubernetesReadRules([]sprooziv1alpha1.KubernetesReadResource{sprooziv1alpha1.KubernetesReadPods})
	if err != nil {
		t.Fatal(err)
	}
	for _, rule := range rules {
		for _, resource := range rule.Resources {
			if resource == "*" || resource == "secrets" || resource == "pods/exec" {
				t.Fatalf("unsafe resource emitted: %q", resource)
			}
		}
		if rule.APIGroups[0] == "" && rule.Resources[0] == "pods" {
			if len(rule.Verbs) != len([]string{"get", "list", "watch"}) {
				t.Fatal("approved rules must be read-only")
			}
		}
	}
}
