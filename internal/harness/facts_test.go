package harness

import (
	"slices"
	"strings"
	"testing"
)

// managedFactProblems names each problem it should, for entries that break
// one rule at a time, and none for a complete entry.
func TestManagedFactProblemsNamesEachBrokenRule(t *testing.T) {
	complete := func() Harness {
		h, err := Managed(NativeCodex)
		if err != nil {
			t.Fatal(err)
		}
		h.SignInChoices = slices.Clone(h.SignInChoices)
		return h
	}
	if problems := managedFactProblems(complete()); len(problems) != 0 {
		t.Fatalf("a complete entry has problems: %v", problems)
	}
	for _, c := range []struct {
		name   string
		change func(*Harness)
		want   string
	}{
		{"no DisplayName", func(h *Harness) { h.DisplayName = "" }, "needs DisplayName"},
		{"no Product", func(h *Harness) { h.Product = "" }, "needs Product"},
		{"relative Executable", func(h *Harness) { h.Executable = "bin/agent" }, "needs Executable"},
		{"no InstructionsFile", func(h *Harness) { h.InstructionsFile = "" }, "needs InstructionsFile"},
		{"no KeyEnv", func(h *Harness) { h.KeyEnv = "" }, "needs KeyEnv"},
		{"no ModelFlag", func(h *Harness) { h.ModelFlag = controlledFlag{} }, "needs ModelFlag"},
		{"a ModelFlag without a value", func(h *Harness) { h.ModelFlag.value = false }, "needs ModelFlag"},
		{"a ConfigOverride without its keys", func(h *Harness) { h.ConfigOverride.ControlledKeys = nil },
			"ConfigOverride's Flag, ControlledKeys and ModelKey together"},
		{"no ConfigOverride", func(h *Harness) { h.ConfigOverride = ConfigOverride{} },
			"needs ConfigOverride, or, for a client with no option that sets one configuration key, an entry with that " +
				"reason in configOverrideExempt"},
		{"no RefusedFlags", func(h *Harness) { h.RefusedFlags = nil }, "needs RefusedFlags"},
		{"no ReservedEnv", func(h *Harness) { h.ReservedEnv = nil }, "needs ReservedEnv"},
		{"no SignInChoices", func(h *Harness) { h.SignInChoices = nil }, "needs SignInChoices"},
		{"TokenEnv without its placeholder", func(h *Harness) { h.TokenEnv = "X_TOKEN" },
			"TokenEnv and TokenPlaceholder together"},
		{"a choice without a label", func(h *Harness) { h.SignInChoices[0].Label = " " }, "needs a Label"},
		{"a choice without an answer", func(h *Harness) { h.SignInChoices[0].Answer = "" }, "needs an Answer"},
		{"an unknown answer", func(h *Harness) { h.SignInChoices[0].Answer = "password" }, "needs an Answer"},
		{"a repeated answer", func(h *Harness) {
			h.SignInChoices = append(h.SignInChoices, SignInChoice{Label: "Again", Answer: AnswerKey})
		}, "repeats an earlier choice"},
		{"a repeated label", func(h *Harness) {
			h.SignInChoices = append(h.SignInChoices, SignInChoice{Label: h.SignInChoices[0].Label, Answer: AnswerToken})
		}, "repeats an earlier choice"},
		{"ChatGPT on an Anthropic harness", func(h *Harness) { h.Protocol = ProtocolAnthropic }, "ChatGPT plan"},
		{"its own sign-in without a sign-in file", func(h *Harness) { h.SignInFile = "" }, "needs its SignInFile"},
	} {
		h := complete()
		c.change(&h)
		problems := managedFactProblems(h)
		if len(problems) != 1 || !strings.Contains(problems[0], c.want) {
			t.Errorf("%s: problems %q; want one naming %q", c.name, problems, c.want)
		}
	}
	agentRun, err := Lookup("opencode")
	if err != nil {
		t.Fatal(err)
	}
	if problems := managedFactProblems(agentRun); len(problems) != 0 {
		t.Errorf("an AgentRun-only entry has problems: %v", problems)
	}
	agentRun.SignInChoices = []SignInChoice{{Label: "x", Answer: AnswerKey}}
	if problems := managedFactProblems(agentRun); len(problems) != 1 || !strings.Contains(problems[0], "no Native kind") {
		t.Errorf("an AgentRun-only entry with SignInChoices: %q; want one problem naming its missing Native kind",
			problems)
	}
	agentRun.SignInChoices, agentRun.ModelFlag = nil, modelFlag
	if problems := managedFactProblems(agentRun); len(problems) != 1 || !strings.Contains(problems[0], "no Native kind") {
		t.Errorf("an AgentRun-only entry with a ModelFlag: %q; want one problem naming its missing Native kind",
			problems)
	}
}

// configOverrideExempt names the managed harnesses that declare no
// ConfigOverride, and why: a harness whose arguments can set one
// configuration key must declare that option, so its controlled keys are
// refused.
var configOverrideExempt = map[NativeKind]string{
	NativeClaude: "its configuration options are --settings and --setting-sources, which RefusedFlags refuses as " +
		"whole options; it has no option that sets one key",
	NativeCopilot: "not established from this repository: its RefusedFlags refuse its MCP and tool options as whole " +
		"options, and Sproozi writes its settings.json and config.json itself, but whether the client has an " +
		"option that sets one configuration key has not been checked",
}

// An exemption names a managed harness that declares no ConfigOverride.
func TestConfigOverrideExemptionsAreCurrent(t *testing.T) {
	for kind := range configOverrideExempt {
		if h, err := Managed(kind); err != nil || h.ConfigOverride.Flag.declared() {
			t.Errorf("configOverrideExempt names %q, which is not a managed harness without a ConfigOverride; "+
				"remove it", kind)
		}
	}
}

// declared reports that the option has a spelling.
func (f controlledFlag) declared() bool { return len(f.long) != 0 || f.short != "" }

// Exactly one managed harness is the default: none and two are each refused,
// and a default on an AgentRun-only entry does not count.
func TestRegistryProblemsRequireOneDefault(t *testing.T) {
	if problems := registryProblems(registry); len(problems) != 0 {
		t.Fatalf("the registry has problems: %v", problems)
	}
	withDefaults := func(defaults ...bool) []Harness {
		entries := slices.Clone(registry)
		managed := 0
		for i := range entries {
			if entries[i].Native != "" {
				entries[i].Default = managed < len(defaults) && defaults[managed]
				managed++
			}
		}
		return entries
	}
	for _, c := range []struct {
		name    string
		entries []Harness
	}{
		{"none", withDefaults()},
		{"two", withDefaults(true, true)},
	} {
		problems := registryProblems(c.entries)
		if len(problems) != 1 || !strings.Contains(problems[0], "exactly one managed harness must be the Default") {
			t.Errorf("%s: problems %q; want one about the default", c.name, problems)
		}
	}
	agentRunDefault := withDefaults()
	for i := range agentRunDefault {
		if agentRunDefault[i].Native == "" {
			agentRunDefault[i].Default = true
			break
		}
	}
	if problems := registryProblems(agentRunDefault); len(problems) != 2 {
		t.Errorf("a default AgentRun-only entry: problems %q; want its managed-fact problem and no default", problems)
	}
}
