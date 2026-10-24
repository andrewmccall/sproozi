package nativeconfig

import "errors"

const (
	nativeClaudeSelector  = "claude-code"
	nativeCopilotSelector = "copilot"
)

// ValidateClaude keeps launch-owned hooks, permission mode and instructions out
// of administrator options. Codex TOML assignments have no Claude interpretation.
func ValidateClaude(model string, options []string) error {
	if len(options) != 0 {
		return errors.New("native options are supported only for Codex")
	}
	return Validate(model, nil)
}

// ValidateHarness is the neutral API/config boundary; it cannot import harness,
// whose native adapters already depend on API resource types.
func ValidateHarness(kind, model string, options []string) error {
	switch kind {
	case "codex":
		return Validate(model, options)
	case nativeClaudeSelector, nativeCopilotSelector:
		// Neither reads Codex TOML options; the model name is shared.
		return ValidateClaude(model, options)
	default:
		return errors.New("unsupported native harness")
	}
}
