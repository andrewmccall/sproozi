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
		{"no variable", nil},
		{"blank OpenAI sign-in token", map[string]string{openAITokenEnv: "\n"}},
		{"blank Anthropic sign-in token", map[string]string{anthropicTokenEnv: " \t\n"}},
		{"both sign-in tokens blank", map[string]string{openAITokenEnv: " ", anthropicTokenEnv: "\n"}},
		{"blank OpenAI API key", map[string]string{openAIKeyEnv: "\n"}},
		{"blank Anthropic API key", map[string]string{anthropicKeyEnv: " \n"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			validGatewayEnvironment(t)
			for _, name := range []string{openAIKeyEnv, anthropicKeyEnv, openAITokenEnv,
				anthropicTokenEnv, copilotTokenEnv, modelAuthModeEnv} {
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
