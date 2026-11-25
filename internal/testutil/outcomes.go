package testutil

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// RecordedOutcomes compares lines, one outcome each, with the recorded file.
// A change that only moves where a rule is kept leaves the file
// byte-identical; regenerate it with SPROOZI_UPDATE_OUTCOMES=1 only for a
// change that means to alter an outcome, and show the difference in that
// change.
func RecordedOutcomes(t testing.TB, file string, lines []string) {
	t.Helper()
	got := []byte(strings.Join(lines, "\n") + "\n")
	if os.Getenv("SPROOZI_UPDATE_OUTCOMES") == "1" {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("%v; record the outcomes with SPROOZI_UPDATE_OUTCOMES=1", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("a recorded outcome changed; %s records what each layer answered. If the change is meant, "+
			"regenerate it with SPROOZI_UPDATE_OUTCOMES=1 and show the difference in its own commit", file)
	}
}
