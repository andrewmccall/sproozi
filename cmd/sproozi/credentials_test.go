package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/localsetup"
)

// "credentials set" with an unknown name lists each accepted name once, though
// copilot is both an agent and a provider.
func TestCredentialsSetNamesEachAcceptedNameOnce(t *testing.T) {
	q := &options{verb: verbSet, operand: "unknown-agent"}
	_, err := q.credentialsCommand(t.Context(), localsetup.Layout{}, localsetup.Installation{})
	if err == nil {
		t.Fatal("an unknown agent or provider was accepted")
	}
	message := err.Error()
	_, list, ok := strings.Cut(message, "credentials set takes ")
	if !ok {
		t.Fatalf("usage error names no accepted names: %q", message)
	}
	seen := map[string]bool{}
	for name := range strings.SplitSeq(list, ", ") {
		if seen[name] {
			t.Errorf("usage error names %q twice: %q", name, message)
		}
		seen[name] = true
	}
	for _, name := range append(harness.ManagedKindNames(), credentialProviders...) {
		if !seen[name] {
			t.Errorf("usage error leaves out %q: %q", name, message)
		}
	}
}

// The credentials help names every accepted name, from the same list.
func TestCredentialsHelpNamesEachAcceptedName(t *testing.T) {
	for _, c := range commands {
		if c.name != cmdCredentials {
			continue
		}
		for _, name := range credentialNames() {
			if !strings.Contains(c.about, name) {
				t.Errorf("credentials help leaves out %q: %q", name, c.about)
			}
		}
		return
	}
	t.Fatal("no credentials command")
}

// An installed harness this release does not know uses no stored
// credential: "credentials list" names it under no provider.
func TestCredentialsListAttributesNoProviderToAnUnknownHarness(t *testing.T) {
	inst := localsetup.Installation{Harnesses: []localsetup.Harness{
		{Harness: harness.NativeKind("not-registered"), Credential: "/credentials/openai"},
		{Harness: harness.NativeCodex, Credential: "/credentials/openai"},
	}}
	q := &options{verb: verbList}
	p, err := q.credentialsCommand(t.Context(), localsetup.Layout{Root: t.TempDir()}, inst)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range p.(credentialReport) {
		for _, h := range e.Harnesses {
			if h == "not-registered" {
				t.Errorf("provider %q lists the unknown harness", e.Provider)
			}
		}
		if e.Provider == providerOpenAI && !slices.Contains(e.Harnesses, string(harness.NativeCodex)) {
			t.Errorf("openai lists %v; want codex", e.Harnesses)
		}
	}
}
