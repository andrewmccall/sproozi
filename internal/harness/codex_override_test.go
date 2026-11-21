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
