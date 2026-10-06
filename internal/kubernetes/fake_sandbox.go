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

	sprooziv1alpha1 "github.com/andrewmccall/sproozi/api/v1alpha1"
)

// FakeSandboxClient is a recording SandboxClient for use in unit tests.
// It records all Delete calls so tests can assert exactly one delete per terminal path.
type FakeSandboxClient struct {
	// Status is returned by GetStatus.
	Status SandboxStatus
	// ExitCode is returned by GetExitCode.
	ExitCode int32
	// EnsureName overrides the returned name; defaults to RunName(run.UID).
	EnsureName string
	// EnsureErr is returned by Ensure if non-nil.
	EnsureErr error
	// StatusErr is returned by GetStatus if non-nil.
	StatusErr error
	// ResultErr is returned by GetExitCode if non-nil.
	ResultErr error
	// DeleteErr is returned by Delete if non-nil.
	DeleteErr error
	// Ensured records the sandbox names returned by Ensure.
	Ensured []string
	// Deleted records the sandbox names passed to Delete.
	Deleted []string
}

// Ensure records the call and returns EnsureName (or RunName(run.UID) if empty).
func (f *FakeSandboxClient) Ensure(_ context.Context, run *sprooziv1alpha1.AgentRun, _ *sprooziv1alpha1.AgentRuntime) (string, error) {
	if f.EnsureErr != nil {
		return "", f.EnsureErr
	}
	name := f.EnsureName
	if name == "" {
		name = RunName(run.UID)
	}
	f.Ensured = append(f.Ensured, name)
	return name, nil
}

// GetStatus returns Status (or SandboxStatusUnknown if zero) and StatusErr.
func (f *FakeSandboxClient) GetStatus(_ context.Context, _ string) (SandboxStatus, error) {
	if f.StatusErr != nil {
		return SandboxStatusUnknown, f.StatusErr
	}
	if f.Status == "" {
		return SandboxStatusUnknown, nil
	}
	return f.Status, nil
}

// GetExitCode returns ExitCode and ResultErr.
func (f *FakeSandboxClient) GetExitCode(_ context.Context, _ string) (int32, error) {
	return f.ExitCode, f.ResultErr
}

// Delete records the name and returns DeleteErr.
func (f *FakeSandboxClient) Delete(_ context.Context, name string) error {
	f.Deleted = append(f.Deleted, name)
	return f.DeleteErr
}

func (f *FakeSandboxClient) DeleteForRun(ctx context.Context, name, _ string) error {
	return f.Delete(ctx, name)
}
