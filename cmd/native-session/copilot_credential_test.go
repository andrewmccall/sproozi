package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// prepareTestCopilot runs prepareCopilotLaunch with a temporary Copilot home
// and MCP file, returning its arguments, environment and the MCP path. Only
// the key and token routes run here: the sign-in route writes the agent
// shell's environment file at its fixed sandbox path.
func prepareTestCopilot(t *testing.T, env []string) (args, out []string, mcp string) {
	t.Helper()
	dir := t.TempDir()
	home, oldHome, oldMCP, oldProfile := filepath.Join(dir, "copilot"), copilotHome, copilotMCP, profileArchive
	copilotHome, copilotMCP, profileArchive = home, filepath.Join(dir, "native", "copilot-mcp.json"),
		filepath.Join(dir, "absent-profile")
	t.Cleanup(func() { copilotHome, copilotMCP, profileArchive = oldHome, oldMCP, oldProfile })
	c := config{Instructions: "instructions", ClientConfig: &harness.NativeClientConfig{MCPGateway: "https://gw.test"}}
	args, out, err := prepareCopilotLaunch(c, "thread-1", env)
	if err != nil {
		t.Fatal(err)
	}
	return args, out, copilotMCP
}

func copilotEnv(t *testing.T, credential harness.NativeCredential) []string {
	t.Helper()
	env, err := harness.NativeBaseEnvironment(harness.NativeCopilot, "uid", credential)
	if err != nil {
		t.Fatal(err)
	}
	return env
}

// A gateway-held key wins when the token variable is also set: Copilot runs
// in bring-your-own-key mode, offline, and the agent's shell gets no
// environment file.
func TestCopilotLaunchPrefersTheKeyToTheToken(t *testing.T) {
	env := copilotEnv(t, harness.CredentialKey)
	env = append(env, copilotEntry.TokenEnv+"="+harness.NativeCopilotTokenPlaceholder)
	if got := copilotCredential(env); got != harness.CredentialKey {
		t.Fatalf("key and token read as %s; want the key", got)
	}
	args, out, _ := prepareTestCopilot(t, env)
	if !slices.Contains(out, "COPILOT_OFFLINE=true") || slices.Contains(args, "--bash-env=on") {
		t.Fatalf("key and token did not launch the key route: args %v env %v", args, out)
	}
}

// A gateway-held token is neither the key route nor the sign-in route: no
// bring-your-own-key provider, and no shell environment file.
func TestCopilotLaunchWithATokenTakesNeitherOtherRoute(t *testing.T) {
	args, out, _ := prepareTestCopilot(t, copilotEnv(t, harness.CredentialToken))
	if slices.Contains(args, "--bash-env=on") || slices.ContainsFunc(out, func(e string) bool {
		return strings.HasPrefix(e, "BASH_ENV=") || strings.HasPrefix(e, "COPILOT_PROVIDER_") || e == "COPILOT_OFFLINE=true"
	}) {
		t.Fatalf("a token launch took another route: args %v env %v", args, out)
	}
}

// The workspace is trusted in the file Copilot keeps its sign-in in, the
// registry's SignInFile, and Copilot is pointed at the MCP file the launch
// wrote, whose name is the registry's MCP file name in the launch directory.
func TestCopilotLaunchWritesTheRegistryFiles(t *testing.T) {
	launchMCP := copilotMCP
	if want := "/tmp/sproozi-native/" + copilotEntry.MCP.File; launchMCP != want {
		t.Errorf("Copilot's MCP launch file is %s; want %s", launchMCP, want)
	}
	args, _, mcp := prepareTestCopilot(t, copilotEnv(t, harness.CredentialKey))
	i := slices.Index(args, "--additional-mcp-config")
	if i < 0 || i+1 >= len(args) || args[i+1] != "@"+mcp {
		t.Fatalf("args %v do not load %s", args, mcp)
	}
	written, err := os.ReadFile(mcp)
	if err != nil || !strings.Contains(string(written), "https://gw.test/mcp/sproozi") {
		t.Fatalf("MCP file %s: %v %s", mcp, err, written)
	}
	data, err := os.ReadFile(filepath.Join(copilotHome, copilotEntry.SignInFile))
	if err != nil {
		t.Fatalf("no %s in the Copilot home: %v", copilotEntry.SignInFile, err)
	}
	var document struct {
		TrustedFolders []string `json:"trustedFolders"`
	}
	if err = json.Unmarshal(data, &document); err != nil || !slices.Contains(document.TrustedFolders, workspace) {
		t.Fatalf("%s does not trust %s: %v %s", copilotEntry.SignInFile, workspace, err, data)
	}
}
