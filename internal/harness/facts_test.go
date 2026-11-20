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
		{"relative Executable", func(h *Harness) { h.Executable = "codex" }, "needs Executable"},
		{"no InstructionsFile", func(h *Harness) { h.InstructionsFile = "" }, "needs InstructionsFile"},
		{"no KeyEnv", func(h *Harness) { h.KeyEnv = "" }, "needs KeyEnv"},
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
}
