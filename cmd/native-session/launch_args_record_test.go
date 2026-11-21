package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/harness"
)

// launchArgumentsRecord holds the full launch arguments each managed
// harness's supervisor renders, with a configured model and approved
// connections, for each recorded list of user harness arguments, beside
// whether the launch contract accepts that list. A change to argument
// validation leaves every rendered list unchanged; regenerate it with
// SPROOZI_UPDATE_OUTCOMES=1 only for a change that means to alter one.
const launchArgumentsRecord = "testdata/launch-arguments.json"

// modelOption is the model option every managed harness takes.
const modelOption = "--model"

// recordedHarnessArgs are user harness arguments: none, the documented
// ones, each way of choosing a model, and the Codex override forms the
// registry outcomes record.
var recordedHarnessArgs = [][]string{
	nil,
	{modelOption, "gpt-5.5", "-c", "model_reasoning_effort=high"},
	{modelOption, "gpt-5.5"}, {"--model=x"}, {"-m", "x"}, {"-mx"}, {modelOption}, {"-m"},
	{"-c", "model=x"}, {"--config=model=x"}, {"-cmodel=x"}, {"-c=model=x"}, {"-c", "-mx"},
	{"-c=model_provider=x"}, {"-c", "--yolo"}, {"-c", "-p", "x"}, {"--config", "--cd=/"}, {"-c"},
	{"--harmless", "-c"}, {"-m", "--sandbox"}, {modelOption, "-c"},
	{"-c", "mcp_servers={}"}, {"-c", `mcp_servers.github.url="https://x"`}, {"-c", `" sandbox_mode"=x`},
	{"-c", "a b=x"}, {"-c", "=x"}, {"-c", "openai_base_url=x"}, {"--add-dir", "/tmp"},
}

// recordedLaunch renders kind's launch arguments for user arguments args.
func recordedLaunch(t *testing.T, kind harness.NativeKind, args []string) []string {
	t.Helper()
	h, err := harness.Managed(kind)
	if err != nil {
		t.Fatal(err)
	}
	clients := &harness.NativeClientConfig{MCPGateway: "https://gateway.test", MCPServers: []string{"github", "docs"}}
	c := config{Harness: kind, Instructions: instructions, InstructionDigest: digest(), ClientConfig: clients,
		Model: "configured-model", Options: h.KeyOptions, Args: args, RolloutMaxBytes: 8192, Env: []string{}}
	switch kind {
	case harness.NativeCodex:
		return codexArguments(c, selected)
	case harness.NativeClaude:
		c.ThreadID = selected
		return claudeArguments(c, agentprofile.Profile{}, false)
	case harness.NativeCopilot:
		dir := t.TempDir()
		oldHome, oldMCP, oldProfile := copilotHome, copilotMCP, profileArchive
		copilotHome, copilotMCP, profileArchive = filepath.Join(dir, "copilot"),
			filepath.Join(dir, "native", "copilot-mcp.json"), filepath.Join(dir, "absent-profile")
		defer func() { copilotHome, copilotMCP, profileArchive = oldHome, oldMCP, oldProfile }()
		env, err := harness.NativeBaseEnvironment(kind, "uid", harness.CredentialKey)
		if err != nil {
			t.Fatal(err)
		}
		rendered, _, err := prepareCopilotLaunch(c, selected, env)
		if err != nil {
			t.Fatal(err)
		}
		for i := range rendered {
			rendered[i] = strings.ReplaceAll(rendered[i], dir, "<launch files>")
		}
		return rendered
	}
	t.Fatalf("no recorded launch for %s; add a case to recordedLaunch in cmd/native-session/launch_args_record_test.go "+
		"that renders its launch arguments through its adapter's prepare", kind)
	return nil
}

// Every managed harness's rendered launch arguments are the recorded ones.
func TestLaunchArgumentsAreUnchanged(t *testing.T) {
	rows := make([]map[string]any, 0, len(recordedHarnessArgs)*len(harness.ManagedKinds()))
	for _, kind := range harness.ManagedKinds() {
		for _, args := range recordedHarnessArgs {
			rows = append(rows, map[string]any{"kind": string(kind), "args": args,
				"validate": errText(harness.ValidateNativeArgs(kind, args)), "launch": recordedLaunch(t, kind, args)})
		}
	}
	got, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if os.Getenv("SPROOZI_UPDATE_OUTCOMES") == "1" {
		if err := os.WriteFile(launchArgumentsRecord, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(launchArgumentsRecord)
	if err != nil {
		t.Fatalf("%v; record the launch arguments with SPROOZI_UPDATE_OUTCOMES=1", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("a rendered launch argument list or its validation changed; %s records them. If the change is "+
			"meant, regenerate it with SPROOZI_UPDATE_OUTCOMES=1 and show the difference in its own commit",
			launchArgumentsRecord)
	}
}
