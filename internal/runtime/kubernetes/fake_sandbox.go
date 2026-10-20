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

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

// FakeSandboxClient is a recording SandboxClient for use in unit tests.
// It records all DeleteForRun calls so tests can assert exactly one delete per terminal path.
type FakeSandboxClient struct {
	// Status is returned by GetStatus.
	Status SandboxStatus
	// ExitCode and Result are returned by GetCompletion.
	ExitCode int32
	Result   *sprooziv1alpha1.AgentRunResult
	// StatusErr is returned by GetStatus if non-nil.
	StatusErr error
	// CompletionErr is returned by GetCompletion if non-nil.
	CompletionErr error
	// EnsureErr is returned by Ensure if non-nil, and nothing is recorded.
	EnsureErr error
	// Ensured records the sandbox names returned by Ensure.
	Ensured []string
	// Deleted records the sandbox names passed to DeleteForRun.
	Deleted []string
}

// ValidateRuntime applies the template check; there is no trust bundle to read.
func (f *FakeSandboxClient) ValidateRuntime(_ context.Context, rt *sprooziv1alpha1.AgentRuntime) error {
	if err := rt.Spec.ValidateRuntimeTemplate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidRuntime, err)
	}
	return nil
}

// Ensure records and returns RunName(run.UID).
func (f *FakeSandboxClient) Ensure(_ context.Context, run *sprooziv1alpha1.AgentRun, _ *sprooziv1alpha1.AgentRuntime) (string, error) {
	if f.EnsureErr != nil {
		return "", f.EnsureErr
	}
	name := RunName(run.UID)
	f.Ensured = append(f.Ensured, name)
	return name, nil
}

// GetStatus returns Status (or SandboxStatusAbsent if zero) and StatusErr.
func (f *FakeSandboxClient) GetStatus(_ context.Context, _ string) (SandboxStatus, error) {
	if f.StatusErr != nil {
		return SandboxStatusUnknown, f.StatusErr
	}
	if f.Status == "" {
		return SandboxStatusAbsent, nil
	}
	return f.Status, nil
}

// GetCompletion returns the configured terminal observation and CompletionErr.
func (f *FakeSandboxClient) GetCompletion(_ context.Context, _, _ string) (SandboxCompletion, error) {
	return SandboxCompletion{ExitCode: f.ExitCode, Result: f.Result}, f.CompletionErr
}

// DeleteForRun records the name.
func (f *FakeSandboxClient) DeleteForRun(_ context.Context, name, _ string) error {
	f.Deleted = append(f.Deleted, name)
	return nil
}
