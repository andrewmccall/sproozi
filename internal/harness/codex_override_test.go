package harness

import (
	"strings"
	"testing"
)

// refused fails t unless ValidateNativeArgs refuses args for kind.
func refused(t *testing.T, kind NativeKind, args ...string) {
	t.Helper()
	if err := ValidateNativeArgs(kind, args); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("%s %q accepted: %v", kind, args, err)
	}
}

// An override or model option given as a separate argument needs a value
// that is not itself an option, and one after it: a missing value would
// swallow the conversation Sproozi adds after the harness arguments.
func TestAnOptionValueIsNeitherAnOptionNorMissing(t *testing.T) {
	for _, args := range [][]string{
		{"-c", "--sandbox"}, {"-c", "--yolo"}, {"-c", "-p", "x"}, {"--config", "--cd=/"}, {"-c", "--"},
		{"-c", "-mx"}, {"-c"}, {"--config"}, {"--harmless", "-c"}, {"-m"}, {"--model"}, {"--harmless", "-m"},
		{"-m", "--sandbox"}, {"--model", "-c"},
	} {
		refused(t, NativeCodex, args...)
	}
	for _, kind := range []NativeKind{NativeClaude, NativeCopilot} {
		refused(t, kind, "--model")
		refused(t, kind, "--model", "--verbose")
	}
	for _, args := range [][]string{{"-c", "x=-1"}, {"--model", "gpt-5.5"}, {"-m", "x"}, {"--model=-x"}} {
		if err := ValidateNativeArgs(NativeCodex, args); err != nil {
			t.Errorf("codex %q refused: %v", args, err)
		}
	}
}

// An override of a table that holds a controlled key would replace it, so
// it is refused as the key itself is.
func TestAParentOfAControlledKeyIsRefused(t *testing.T) {
	for _, key := range codexConfigOverride.ControlledKeys {
		parts := strings.Split(key, ".")
		for n := 1; n < len(parts); n++ {
			parent := strings.Join(parts[:n], ".")
			refused(t, NativeCodex, "-c", parent+"={}")
			refused(t, NativeCodex, "-c", parent+`={sproozi={url="https://x"}}`)
		}
	}
}

// A controlled key is refused however TOML spells it: quoted, literal,
// escaped or spaced. A key that is not a TOML dotted key is refused too;
// case is kept, since TOML keys are case-sensitive.
func TestEveryTOMLSpellingOfAControlledKeyIsRefused(t *testing.T) {
	for _, value := range []string{
		`"sandbox_mode"=x`, `'sandbox_mode'=x`, `" sandbox_mode"=x`, `"sandbox_mode "=x`,
		"\"sandbox\\" + "u005fmode\"=x", "\"sandbox\\" + "U0000005fmode\"=x", `sandbox_mode =x`, ` sandbox_mode=x`,
		`mcp_servers . "sproozi" . url=x`, `mcp_servers.'sproozi'.url=x`, `"mcp_servers"."sproozi"={}`,
		`a b=x`, `sandbox_mode.=x`, `.sandbox_mode=x`, `"unterminated=x`, `'unterminated=x`,
		`"bad\qescape"=x`, `a..b=x`, `a.=x`, `sandbox_mode x`,
	} {
		refused(t, NativeCodex, "-c", value)
	}
	for _, value := range []string{"SANDBOX_MODE=x", "model_reasoning_effort=high", `"model_reasoning_effort"=high`,
		"shell_environment_policy.inherit=all", "a-b_c.d=1", `"a=b"=x`} {
		if err := ValidateNativeArgs(NativeCodex, []string{"-c", value}); err != nil {
			t.Errorf("codex -c %q refused: %v", value, err)
		}
	}
}

// Every spelling of a Codex override of a controlled key gets the same
// answer, and an override with an empty key is refused.
func TestEveryOverrideSpellingIsRefusedAlike(t *testing.T) {
	for _, key := range codexConfigOverride.ControlledKeys {
		for _, args := range [][]string{
			{"-c", key + "=x"}, {"--config", key + "=x"}, {"-c" + key + "=x"}, {"-c=" + key + "=x"},
			{"--config=" + key + "=x"},
		} {
			refused(t, NativeCodex, args...)
		}
	}
	for _, args := range [][]string{{"-c="}, {"-c==x"}, {"-c", "=x"}, {"--config="}, {"--config", ""}} {
		refused(t, NativeCodex, args...)
	}
}
