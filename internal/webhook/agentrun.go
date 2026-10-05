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

package webhook

import (
	"context"
	"fmt"
	"maps"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

const RunTask = "Process incoming webhook event"

type AgentRunCreator struct {
	Client client.Client
	Now    func() time.Time
}

func (c *AgentRunCreator) Create(ctx context.Context, tmpl *sprooziv1alpha1.AgentTemplate, policy *sprooziv1alpha1.AgentPolicy, eventContext map[string]string) (*sprooziv1alpha1.AgentRun, error) {
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}

	run := &sprooziv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%d", tmpl.Name, now().UnixNano()),
			Namespace: tmpl.Namespace,
		},
		Spec: sprooziv1alpha1.AgentRunSpec{
			TemplateRef: sprooziv1alpha1.AgentTemplateReference{Name: tmpl.Name},
			Task:        RunTask,
			Capabilities: append([]sprooziv1alpha1.CapabilityKind(nil),
				policy.Spec.AllowedCapabilities...),
			EventContext: copyStringMap(eventContext),
		},
	}
	if err := c.Client.Create(ctx, run); err != nil {
		return nil, fmt.Errorf("webhook: create AgentRun: %w", err)
	}
	return run, nil
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}
