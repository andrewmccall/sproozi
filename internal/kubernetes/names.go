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
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func ownedByRun(obj metav1.Object, uid string) bool {
	return obj.GetLabels()[ManagedByLabel] == ManagedByValue && obj.GetLabels()[RunUIDLabel] == uid
}

const (
	// AgentsNamespace is the namespace where per-run identities live.
	AgentsNamespace = "sproozi-agents"

	// RunLabel is applied to every resource managed by Sproozi for a run.
	// Its value is the AgentRun name.
	RunLabel = "sproozi.com/agentrun"

	// RunNamespaceLabel records the namespace of the AgentRun that owns a resource.
	// Required for cross-namespace Pod-to-AgentRun mapping (sandbox Pods live in
	// AgentsNamespace while AgentRuns may be in any namespace).
	RunNamespaceLabel = "sproozi.com/agentrun-namespace"

	// RunUIDLabel records the immutable AgentRun UID. It binds disposable
	// cross-namespace resources to one run even when a run name is reused.
	RunUIDLabel = "sproozi.com/agentrun-uid"

	// ManagedByLabel identifies the controller that owns a resource.
	ManagedByLabel = "sproozi.com/managed-by"

	// ManagedByValue is the value of ManagedByLabel for controller-owned resources.
	ManagedByValue = "sproozi-controller"
)

// RunName derives a deterministic, DNS-label-safe resource name from an AgentRun UID.
// The UID (UUID format) is stripped of hyphens and prefixed with "sproozi-",
// producing a 40-character name that is unique per run and contains no task/event text.
func RunName(uid types.UID) string {
	return "sproozi-" + strings.ToLower(strings.ReplaceAll(string(uid), "-", ""))
}
