package harness

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

// outcomesFile records what every per-harness fact's readers return for each
// registered selector, an unregistered one and the empty kind. A change that
// only moves where a fact is kept leaves it byte-identical; regenerate it with
// SPROOZI_UPDATE_OUTCOMES=1 only for a change that means to alter an outcome,
// and show the difference in that change.
const outcomesFile = "testdata/registry-outcomes.json"

// outcomeKinds are the kinds whose outcomes are recorded: every registered
// selector, in registry order, then an unregistered kind and the empty one.
func outcomeKinds() []NativeKind {
	var kinds []NativeKind
	for _, s := range Selectors() {
		kinds = append(kinds, NativeKind(s))
	}
	return append(kinds, "not-registered", "")
}

// outcomeArgs are harness arguments that exercise every harness's refused
// flags and model selection: every spelling any harness controls, plus the
// model spellings.
func outcomeArgs() [][]string {
	seen := map[string]bool{}
	var out [][]string
	add := func(args ...string) {
		key, _ := json.Marshal(args)
		if !seen[string(key)] {
			seen[string(key)] = true
			out = append(out, args)
		}
	}
	for _, table := range [][]controlledFlag{codexControlled, claudeControlled, copilotControlled} {
		for _, f := range table {
			for _, name := range f.long {
				add("--" + name)
				add("--" + name + "=x")
			}
			if f.short != "" {
				add("-" + f.short)
			}
		}
	}
	add("--model", "x")
	add("--model=x")
	add("-m", "x")
	add("-mx")
	add("-c", "model=x")
	add("-c", "model_provider=x")
	add("--config", "projects.x=y")
	add("--")
	add("--harmless")
	return out
}

func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// recordedOutcomes computes, through each fact's public reader, what Sproozi
// does today for every outcome kind.
func recordedOutcomes() map[string]any {
	reservedCandidates := []string{"HOME", "PWD", "KUBECONFIG", "PATH", "GH_TOKEN", "CODEX_HOME", "CLAUDE_CONFIG_DIR",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC", "DISABLE_AUTOUPDATER", "CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION",
		"CLAUDE_CODE_DISABLE_BACKGROUND_TASKS", "COPILOT_HOME", "COPILOT_AUTO_UPDATE", "OPENAI_API_KEY",
		"ANTHROPIC_API_KEY", "COPILOT_PROVIDER_API_KEY", "COPILOT_GITHUB_TOKEN"}
	reserved := map[string]bool{}
	for _, key := range reservedCandidates {
		reserved[key] = reservedEnvironment(key)
	}
	perKind := []map[string]any{}
	for _, kind := range outcomeKinds() {
		o := map[string]any{"kind": string(kind)}
		o["validate"] = errText(kind.Validate())
		o["takesTask"] = kind.TakesTask()
		o["remoteExecutor"] = RemoteExecutorKind(kind)
		o["provesCompletion"] = ProvesCompletion(kind)
		o["instructionsFile"] = NativeUserInstructionsFile(kind)
		o["displayName"] = NativeDisplayName(kind)
		o["executable"] = NativeExecutable(kind)
		o["homePath"] = NativeHomePath(kind)
		directory, err := NativeDirectory(kind)
		o["nativeDirectory"] = []string{directory, errText(err)}
		h, err := Managed(kind)
		o["managed"] = errText(err)
		o["product"] = h.Product
		o["keyEnv"] = h.KeyEnv
		o["tokenEnv"] = []string{h.TokenEnv, h.TokenPlaceholder}
		o["signInYieldsGitHubToken"] = h.SignInYieldsGitHubToken
		o["reservedEnv"] = h.ReservedEnv
		o["keyOptions"] = h.KeyOptions
		o["signInOptions"] = h.SignInOptions
		o["signInFile"] = h.SignInFile
		o["provider"] = []string{h.Provider(false), h.Provider(true)}
		o["inspectionHost"] = h.Protocol.InspectionHost()
		o["default"] = h.Default
		env := map[string]any{}
		for _, credential := range []NativeCredential{CredentialKey, CredentialToken, CredentialSignIn, "other"} {
			values, err := NativeBaseEnvironment(kind, "uid", credential)
			env[string(credential)] = map[string]any{"env": values, "error": errText(err)}
		}
		o["baseEnvironment"] = env
		refused := map[string]string{}
		selectsModel := map[string]bool{}
		for _, args := range outcomeArgs() {
			key, _ := json.Marshal(args)
			refused[string(key)] = errText(ValidateNativeArgs(kind, args))
			selectsModel[string(key)] = NativeArgsSelectModel(kind, args)
		}
		o["refusedArgs"] = refused
		o["argsSelectModel"] = selectsModel
		perKind = append(perKind, o)
	}
	return map[string]any{
		"default":         string(Default()),
		"taskKindList":    TaskKindList(),
		"managedKindList": ManagedKindList(),
		"managedKinds":    ManagedKindNames(),
		"selectors":       Selectors(),
		"reservedEnv":     reserved,
		"kinds":           perKind,
	}
}

// Each per-harness fact's readers return what they returned before the fact
// moved into the registry. The file is the evidence: a refactor that moves a
// fact leaves it unchanged.
func TestRegistryOutcomesAreUnchanged(t *testing.T) {
	got, err := json.MarshalIndent(recordedOutcomes(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if os.Getenv("SPROOZI_UPDATE_OUTCOMES") == "1" {
		if err := os.WriteFile(outcomesFile, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(outcomesFile)
	if err != nil {
		t.Fatalf("%v; record the outcomes with SPROOZI_UPDATE_OUTCOMES=1", err)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("a per-harness outcome changed; %s records what each reader returned. If the change is meant, "+
			"regenerate it with SPROOZI_UPDATE_OUTCOMES=1 and show the difference in its own commit", outcomesFile)
	}
}
