package harness

import (
	"strings"
	"testing"
)

// Arguments the argument tests share.
const (
	argCdRoot           = "--cd=/"
	argHarmless         = "--harmless"
	argSandbox          = "--sandbox"
	argYolo             = "--yolo"
	argConfig           = "--config"
	quotedSandboxMode   = `"sandbox_mode"=x`
	reasoningEffortHigh = "model_reasoning_effort=high"
	valueWithoutKey     = "foo"
)

// A key part tomlQuote spells reads back as itself.
func TestTOMLQuotedKeyPartsReadBack(t *testing.T) {
	for _, part := range []string{"plain", "brand new", `with "quotes"`, `back\slash`, "a.b", "tab\there", "é", "x\x7fy"} {
		parts, rest, ok := tomlDottedKey(tomlQuote(part) + "=x")
		if !ok || rest != "=x" || len(parts) != 1 || parts[0] != part {
			t.Errorf("%q spelled %s read back as %q, %q, %v", part, tomlQuote(part), parts, rest, ok)
		}
	}
}

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
		{"-c", argSandbox}, {"-c", argYolo}, {"-c", "-p", "x"}, {argConfig, argCdRoot}, {"-c", "--"},
		{"-c", "-mx"}, {"-c"}, {argConfig}, {argHarmless, "-c"}, {"-m"}, {nativeModelFlag}, {argHarmless, "-m"},
		{"-m", argSandbox}, {nativeModelFlag, "-c"},
	} {
		refused(t, NativeCodex, args...)
	}
	for _, kind := range []NativeKind{NativeClaude, NativeCopilot} {
		refused(t, kind, nativeModelFlag)
		refused(t, kind, nativeModelFlag, "--verbose")
	}
	for _, args := range [][]string{{"-c", "x=-1"}, {nativeModelFlag, "gpt-5.5"}, {"-m", "x"}, {"--model=-x"}} {
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
		quotedSandboxMode, `'sandbox_mode'=x`, `" sandbox_mode"=x`, `"sandbox_mode "=x`,
		"\"sandbox\\" + "u005fmode\"=x", "\"sandbox\\" + "U0000005fmode\"=x", `sandbox_mode =x`, ` sandbox_mode=x`,
		`mcp_servers . "sproozi" . url=x`, `mcp_servers.'sproozi'.url=x`, `"mcp_servers"."sproozi"={}`,
		`a b=x`, `sandbox_mode.=x`, `.sandbox_mode=x`, `"unterminated=x`, `'unterminated=x`,
		`"bad\qescape"=x`, `a..b=x`, `a.=x`, `sandbox_mode x`,
	} {
		refused(t, NativeCodex, "-c", value)
	}
	for _, value := range []string{"SANDBOX_MODE=x", reasoningEffortHigh, `"model_reasoning_effort"=high`,
		"shell_environment_policy.inherit=all", "a-b_c.d=1", `"a=b"=x`} {
		if err := ValidateNativeArgs(NativeCodex, []string{"-c", value}); err != nil {
			t.Errorf("codex -c %q refused: %v", value, err)
		}
	}
}

// A Codex override sets a key to a value, so one with no "=" is refused in
// every spelling.
func TestAnOverrideWithoutAValueIsRefused(t *testing.T) {
	for _, args := range [][]string{
		{"-c", valueWithoutKey}, {"-cm"}, {argConfig, valueWithoutKey}, {"--config=foo"}, {"-c=foo"}, {"-c", "sandbox_mode"},
		{"-c", `"quoted"`}, {"-c", "a.b "},
	} {
		refused(t, NativeCodex, args...)
	}
}

// Every spelling of a Codex override of a controlled key gets the same
// answer, and an override with an empty key is refused.
func TestEveryOverrideSpellingIsRefusedAlike(t *testing.T) {
	for _, key := range codexConfigOverride.ControlledKeys {
		for _, args := range [][]string{
			{"-c", key + "=x"}, {argConfig, key + "=x"}, {"-c" + key + "=x"}, {"-c=" + key + "=x"},
			{"--config=" + key + "=x"},
		} {
			refused(t, NativeCodex, args...)
		}
	}
	for _, args := range [][]string{{"-c="}, {"-c==x"}, {"-c", "=x"}, {"--config="}, {argConfig, ""}} {
		refused(t, NativeCodex, args...)
	}
}
