package main

import (
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// The supervisor reads Copilot's credential from the environment that
// NativeBaseEnvironment wrote for that credential, so the writer and the
// reader agree on the registry's variables.
func TestCopilotCredentialReadsTheBaseEnvironment(t *testing.T) {
	for _, credential := range []harness.NativeCredential{
		harness.CredentialKey, harness.CredentialToken, harness.CredentialSignIn,
	} {
		env, err := harness.NativeBaseEnvironment(harness.NativeCopilot, "uid", credential)
		if err != nil {
			t.Fatal(err)
		}
		if got := copilotCredential(env); got != credential {
			t.Errorf("environment for %s read as %s: %v", credential, got, env)
		}
	}
}
