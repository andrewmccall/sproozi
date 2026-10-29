package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/localsetup"
	"github.com/andrewmccall/sproozi/internal/modelauth"
	"golang.org/x/term"
)

// choice is one numbered answer to a setup question.
type choice struct {
	label string
	apply func(*options)
}

// interactive reports a person at the terminal who can answer setup's
// questions: standard input and standard error are terminals and no JSON
// result was asked for.
func (q *options) interactive() bool {
	return !q.json && term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

// credentialChosen reports a flag that already says how the agent signs in.
func (q *options) credentialChosen() bool {
	return q.signIn || q.token || q.chatgpt || q.credentialFile != "" || q.credentialStdin
}

// wizard asks for the agent and how it signs in, for each that no flag
// chose. inst is the installation so far, so an agent already set up can
// keep its current choice.
func (q *options) wizard(in *bufio.Reader, w io.Writer, inst localsetup.Installation) error {
	if q.harness == "" {
		kinds := harness.ManagedKinds()
		choices := make([]choice, 0, len(kinds))
		for _, kind := range kinds {
			label := agentNames[kind]
			if label == "" {
				label = string(kind)
			}
			if configured(inst, kind) != nil {
				label += " (already set up)"
			}
			choices = append(choices, choice{label, func(q *options) { q.harness = string(kind) }})
		}
		if err := ask(in, w, "Which agent do you want to set up?", choices, q); err != nil {
			return err
		}
	}
	if q.credentialChosen() {
		return nil
	}
	kind := harness.NativeKind(q.harness)
	choices := signInChoices[kind]
	if len(choices) == 0 {
		return nil
	}
	if current := configured(inst, kind); current != nil {
		keep := choice{"Keep what it uses now: " + mechanismName(*current), func(*options) {}}
		choices = append([]choice{keep}, choices...)
	}
	return ask(in, w, fmt.Sprintf("How should %s sign in to its model provider?", kind), choices, q)
}

// agentNames are the names the wizard shows for each agent.
var agentNames = map[harness.NativeKind]string{
	harness.NativeCodex:   "codex: OpenAI Codex CLI",
	harness.NativeClaude:  "claude-code: Anthropic Claude Code",
	harness.NativeCopilot: "copilot: GitHub Copilot CLI",
}

func signIn(q *options)   { q.signIn = true }
func useToken(q *options) { q.token = true }
func apiKey(*options)     {}

// signInChoices are the ways each agent can reach its provider, most common
// first (docs/guides/local-sessions.md#choose-how-an-agent-signs-in).
var signInChoices = map[harness.NativeKind][]choice{
	harness.NativeCodex: {
		{"An OpenAI API key, held by the gateway", apiKey},
		{"A ChatGPT plan, signed in now in your browser and held by the gateway", func(q *options) { q.chatgpt = true }},
		{"Codex signs in itself with ChatGPT in its first session", signIn},
	},
	harness.NativeClaude: {
		{"An Anthropic API key, held by the gateway", apiKey},
		{"A sign-in token from \"claude setup-token\", held by the gateway", useToken},
		{"Claude Code signs in itself with /login in its first session", signIn},
	},
	harness.NativeCopilot: {
		{"Copilot signs in itself with /login and your Copilot plan in its first session", signIn},
		{"A GitHub token with Copilot access, held by the gateway", useToken},
		{"An OpenAI API key, held by the gateway (no Copilot plan needed)", apiKey},
	},
}

func configured(inst localsetup.Installation, kind harness.NativeKind) *localsetup.Harness {
	for i := range inst.Harnesses {
		if inst.Harnesses[i].Harness == kind {
			return &inst.Harnesses[i]
		}
	}
	return nil
}

func mechanismName(h localsetup.Harness) string {
	switch {
	case h.Kind == modelauth.ChatGPTMode:
		return "a ChatGPT plan held by the gateway"
	case h.Mechanism() == harness.CredentialSignIn:
		return "its own sign-in"
	case h.Mechanism() == harness.CredentialToken:
		return "a sign-in token held by the gateway"
	}
	return "an API key held by the gateway"
}

// ask shows a numbered question, with the first choice as the default, and
// applies the answer to q. It asks again after an answer it doesn't know.
func ask(in *bufio.Reader, w io.Writer, question string, choices []choice, q *options) error {
	_, _ = fmt.Fprintln(w, question)
	for i, c := range choices {
		_, _ = fmt.Fprintf(w, "  %d) %s\n", i+1, c.label)
	}
	for {
		_, _ = fmt.Fprintf(w, "Choose 1-%d [1]: ", len(choices))
		line, err := in.ReadString('\n')
		answer := strings.TrimSpace(line)
		if err != nil && (answer == "" || !errors.Is(err, io.EOF)) {
			if errors.Is(err, io.EOF) {
				return errors.New("setup stopped before an answer; give --harness, and --sign-in, --token or a key, " +
					"to run it without questions")
			}
			return err
		}
		n := 1
		if answer != "" {
			n, err = strconv.Atoi(answer)
		}
		if err == nil && n >= 1 && n <= len(choices) {
			choices[n-1].apply(q)
			_, _ = fmt.Fprintln(w)
			return nil
		}
		_, _ = fmt.Fprintf(w, "Type a number from 1 to %d, or press Enter for 1.\n", len(choices))
	}
}
