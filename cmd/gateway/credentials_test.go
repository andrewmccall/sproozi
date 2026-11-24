package main

import (
	"strings"
	"testing"
)

// A Secret key that holds only a newline reaches the gateway as a blank
// value. A blank credential is no credential, so api_key mode must still ask
// for one.
func TestBlankCredentialsDoNotSatisfyAPIKeyMode(t *testing.T) {
	for _, tc := range []struct {
		name   string
		values map[string]string
	}{
		{"unset", nil},
		{"blank OpenAI sign-in token", map[string]string{"OPENAI_SIGN_IN_TOKEN": "\n"}},
		{"blank Anthropic sign-in token", map[string]string{"ANTHROPIC_SIGN_IN_TOKEN": " \t\n"}},
		{"both sign-in tokens blank", map[string]string{"OPENAI_SIGN_IN_TOKEN": " ", "ANTHROPIC_SIGN_IN_TOKEN": "\n"}},
		{"blank OpenAI API key", map[string]string{"OPENAI_API_KEY": "\n"}},
		{"blank Anthropic API key", map[string]string{"ANTHROPIC_API_KEY": " \n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validGatewayEnvironment(t)
			for _, name := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "OPENAI_SIGN_IN_TOKEN",
				"ANTHROPIC_SIGN_IN_TOKEN", "COPILOT_SIGN_IN_TOKEN", "MODEL_AUTH_MODE"} {
				t.Setenv(name, "")
			}
			for name, value := range tc.values {
				t.Setenv(name, value)
			}
			_, err := loadStartupConfig()
			if err == nil || !strings.Contains(err.Error(), "OPENAI_API_KEY or ANTHROPIC_API_KEY is required") {
				t.Fatalf("a gateway with no model credential started: %v", err)
			}
		})
	}
}
