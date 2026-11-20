package main

import (
	"bufio"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/localsetup"
)

func runWizard(t *testing.T, q *options, answers string, inst localsetup.Installation) string {
	t.Helper()
	var out strings.Builder
	if err := q.wizard(bufio.NewReader(strings.NewReader(answers)), &out, inst); err != nil {
		t.Fatalf("wizard: %v\n%s", err, out.String())
	}
	return out.String()
}

func TestWizardAsksForAgentAndSignIn(t *testing.T) {
	q := &options{}
	out := runWizard(t, q, "3\n\n", localsetup.Installation{})
	if q.harness != string(harness.NativeCopilot) || !q.signIn || q.token {
		t.Fatalf("harness %q, sign-in %v, token %v; want copilot with its own sign-in\n%s", q.harness, q.signIn, q.token, out)
	}
	if !strings.Contains(out, "Which agent") || !strings.Contains(out, "How should copilot sign in") {
		t.Fatalf("wizard asked:\n%s", out)
	}
}

func TestWizardAsksAgainAfterAnUnknownAnswer(t *testing.T) {
	q := &options{}
	out := runWizard(t, q, "openai\n9\n2\n2\n", localsetup.Installation{})
	if q.harness != string(harness.NativeClaude) || !q.token {
		t.Fatalf("harness %q, token %v; want claude-code with a token\n%s", q.harness, q.token, out)
	}
	if strings.Count(out, "Type a number") != 2 {
		t.Fatalf("want two retries:\n%s", out)
	}
}

func TestWizardSkipsQuestionsTheFlagsAnswer(t *testing.T) {
	q := &options{harness: string(harness.NativeCopilot), token: true}
	if out := runWizard(t, q, "", localsetup.Installation{}); out != "" {
		t.Fatalf("wizard asked with every flag given:\n%s", out)
	}
	q = &options{credentialFile: "/key"}
	out := runWizard(t, q, "1\n", localsetup.Installation{})
	if q.harness != string(harness.NativeCodex) || strings.Contains(out, "sign in") {
		t.Fatalf("harness %q; the key file already says how it signs in\n%s", q.harness, out)
	}
}

func TestWizardKeepsAConfiguredAgentsChoice(t *testing.T) {
	inst := localsetup.Installation{Harnesses: []localsetup.Harness{{Harness: harness.NativeCopilot}}}
	q := &options{harness: string(harness.NativeCopilot)}
	out := runWizard(t, q, "\n", inst)
	if q.credentialChosen() || !strings.Contains(out, "Keep what it uses now: its own sign-in") {
		t.Fatalf("want the current sign-in kept:\n%s", out)
	}
}

func TestWizardStopsAtEndOfInput(t *testing.T) {
	q := &options{}
	err := q.wizard(bufio.NewReader(strings.NewReader("")), &strings.Builder{}, localsetup.Installation{})
	if err == nil || !strings.Contains(err.Error(), "--harness") {
		t.Fatalf("err = %v; want advice to give the flags", err)
	}
}
