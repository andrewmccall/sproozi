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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

// EnsureServiceAccount creates the per-run ServiceAccount in sproozi-agents if
// it does not already exist. Returns the ServiceAccount name. Idempotent.
func EnsureServiceAccount(ctx context.Context, c client.Client, run *sprooziv1alpha1.AgentRun) (string, error) {
	saName := RunName(run.UID)
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: AgentsNamespace,
			Labels: map[string]string{
				RunLabel:          run.Name,
				RunNamespaceLabel: run.Namespace,
				RunUIDLabel:       string(run.UID),
				ManagedByLabel:    ManagedByValue,
			},
		},
	}
	if err := c.Create(ctx, sa); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return "", err
		}
		var existing corev1.ServiceAccount
		if getErr := c.Get(ctx, client.ObjectKey{Namespace: AgentsNamespace, Name: saName}, &existing); getErr != nil {
			return "", getErr
		}
		if existing.Labels[ManagedByLabel] != ManagedByValue ||
			existing.Labels[RunLabel] != run.Name ||
			existing.Labels[RunNamespaceLabel] != run.Namespace ||
			existing.Labels[RunUIDLabel] != string(run.UID) {
			return "", fmt.Errorf("existing ServiceAccount %s has different run identity", saName)
		}
	}
	return saName, nil
}

// RevokeServiceAccount deletes the named ServiceAccount from sproozi-agents.
// Not-found is treated as success (idempotent cleanup).
func RevokeServiceAccount(ctx context.Context, c client.Client, saName string, expectedUID ...string) error {
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: AgentsNamespace,
		},
	}
	var existing corev1.ServiceAccount
	if err := c.Get(ctx, client.ObjectKeyFromObject(sa), &existing); err != nil {
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
