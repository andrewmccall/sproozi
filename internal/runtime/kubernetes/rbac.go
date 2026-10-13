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
	"fmt"
	"slices"

	rbacv1 "k8s.io/api/rbac/v1"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

var kubernetesReadResourceGroups = map[sprooziv1alpha1.KubernetesReadResource]string{
	sprooziv1alpha1.KubernetesReadPods:         "",
	sprooziv1alpha1.KubernetesReadPodLogs:      "",
	sprooziv1alpha1.KubernetesReadEvents:       "",
	sprooziv1alpha1.KubernetesReadServices:     "",
	sprooziv1alpha1.KubernetesReadDeployments:  appsAPIGroup,
	sprooziv1alpha1.KubernetesReadReplicaSets:  appsAPIGroup,
	sprooziv1alpha1.KubernetesReadStatefulSets: appsAPIGroup,
}

var kubernetesReadVerbs = []string{"get", "list", "watch"}

// KubernetesReadRules compiles the exact safe resource set in a policy scope.
// It never emits wildcard resources, writes, sensitive core resources, or command
// subresources. Duplicate and unknown resources fail closed.
func KubernetesReadRules(resources []sprooziv1alpha1.KubernetesReadResource) ([]rbacv1.PolicyRule, error) {
	if len(resources) == 0 {
		return nil, fmt.Errorf("kubernetes.read scope has no resources")
	}
	byGroup := map[string][]string{}
	seen := map[sprooziv1alpha1.KubernetesReadResource]struct{}{}
	for _, resource := range resources {
		group, allowed := kubernetesReadResourceGroups[resource]
		if !allowed {
			return nil, fmt.Errorf("kubernetes.read resource %q is not safe", resource)
		}
		if _, duplicate := seen[resource]; duplicate {
			return nil, fmt.Errorf("kubernetes.read resource %q is duplicated", resource)
		}
		seen[resource] = struct{}{}
		byGroup[group] = append(byGroup[group], string(resource))
	}
	rules := make([]rbacv1.PolicyRule, 0, len(byGroup))
	for _, group := range []string{"", appsAPIGroup} {
		resources := byGroup[group]
		if len(resources) == 0 {
			continue
		}
		slices.Sort(resources)
		rules = append(rules, rbacv1.PolicyRule{APIGroups: []string{group}, Resources: resources, Verbs: slices.Clone(kubernetesReadVerbs)})
	}
	return rules, nil
}
