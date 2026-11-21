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
