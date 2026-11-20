package main

import (
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
	for _, name := range strings.Split(list, ", ") {
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
