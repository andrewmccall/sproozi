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
package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
	k8sresources "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
)

// ensureSandbox creates the Sandbox Pod if not yet provisioned and stores its name
// in run.Status.Identity.SandboxName. It is idempotent: if SandboxName is already set
// it returns immediately without calling the SandboxClient.
func (r *AgentRunReconciler) ensureSandbox(
	ctx context.Context,
	run *sprooziv1alpha1.AgentRun,
	rt *sprooziv1alpha1.AgentRuntime,
	binding *registry.Record,
) (string, error) {
	if run.Status.Identity.SandboxName != "" {
		return run.Status.Identity.SandboxName, nil
	}

	var name string
	var err error
	if binding != nil {
		managed, supported := r.SandboxClient.(k8sresources.ManagedSandboxClient)
		if !supported {
			return "", fmt.Errorf("sandbox client cannot provision a required session binding")
		}
		name, err = managed.EnsureManaged(ctx, run, rt, binding)
	} else {
		name, err = r.SandboxClient.Ensure(ctx, run, rt)
	}
	if err != nil {
		return "", err
	}

	// Record the name even if another writer changed the run meanwhile. A lost
	// write sends every later reconcile back through Ensure, which compares the
	// existing Pod with the current runtime and fails for good once they differ.
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		run.Status.Identity.SandboxName = name
		updateErr := r.Status().Update(ctx, run)
		if !apierrors.IsConflict(updateErr) {
			return updateErr
		}
		var latest sprooziv1alpha1.AgentRun
		if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(run), &latest); err != nil {
			return err
		}
		if latest.UID != run.UID || IsTerminal(latest.Status.Phase) || !latest.DeletionTimestamp.IsZero() {
			return fmt.Errorf("AgentRun %s changed while its sandbox was recorded", run.Name)
		}
		*run = latest
		return updateErr
	})
	if err != nil {
		return "", err
	}
	return name, nil
}

// processSandboxExit maps the agent container's actual process exit code to a
// terminal AgentRun phase. A workload's own command is the completion
// interface: Sproozi does not infer success from a PR, log line, or model text.
func (r *AgentRunReconciler) processSandboxExit(
	ctx context.Context,
	run *sprooziv1alpha1.AgentRun,
	sandboxName string,
) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	completion, completionErr := r.SandboxClient.GetCompletion(ctx, sandboxName, string(run.UID))
	// A cancel or concurrent terminal update may arrive while the Pod is read.
	// Re-fetch before the status write, retaining UID and resource-version checks.
	var latest sprooziv1alpha1.AgentRun
	if err := r.Get(ctx, client.ObjectKeyFromObject(run), &latest); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if latest.UID != run.UID || IsTerminal(latest.Status.Phase) || !latest.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	run = &latest
	if run.Spec.Cancel {
		run.Status.Result = nil
		return r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseCancelled, "CancelRequested", "")
	}
	if completionErr != nil {
		logger.Info("Could not determine Sandbox container exit code", "name", run.Name, "error", completionErr)
		run.Status.Result = nil
		_, transitionErr := r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseFailed, "SandboxExitUnavailable", "")
		return ctrl.Result{}, transitionErr
	}
	// Persist result with terminal phase in one status update before cleanup.
	// This data never participates in the lifecycle success decision.
	run.Status.Result = completion.Result
	if completion.ExitCode != 0 {
		logger.Info("Sandbox container exited unsuccessfully", "name", run.Name, "exitCode", completion.ExitCode)
		_, err := r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseFailed, "SandboxExitNonZero", "")
		return ctrl.Result{}, err
	}
	logger.Info("Sandbox container exited successfully", "name", run.Name)
	_, err := r.transitionPhase(ctx, run, sprooziv1alpha1.AgentRunPhaseSucceeded, "ExecutionSucceeded", "")
	return ctrl.Result{}, err
}
