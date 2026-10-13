package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
)

func TestActualNativeVersionGate(t *testing.T) {
	for _, tc := range []struct {
		name, script string
		accept       bool
	}{
		{"approved", "printf 'codex-cli 0.161.0\\n'", true},
		{"different version", "printf 'codex-cli 0.162.0\\n'", false},
		{"extra output", "printf 'codex-cli 0.161.0\\nextra\\n'", false},
		{"exit failure", "printf 'codex-cli 0.161.0\\n'; exit 1", false},
		{"oversized", "printf '%0200d' 0", false},
		{"expired deadline", "printf 'codex-cli 0.161.0\\n'", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "native-version")
			if err := os.WriteFile(path, []byte("#!/bin/sh\n"+tc.script+"\n"), 0700); err != nil {
				t.Fatal(err)
			}
			// Admission has its own five-second observation bound. Leave enough
			// launch horizon for the actual process when the full suite runs.
			deadline := time.Now().Add(10 * time.Second)
			if tc.name == "expired deadline" {
				deadline = time.Now().Add(-time.Second)
			}
			err := verifyNativeVersion(context.Background(), harness.NativeCodex,
				path, dir, []string{testPathEnvironment}, deadline)
			if (err == nil) != tc.accept {
				t.Fatalf("version admission: accepted=%v error=%v", tc.accept, err)
			}
		})
	}
}

func TestClaudeActualNativeVersionGate(t *testing.T) {
	for _, output := range []string{"2.1.27 (Claude Code)", "2.1.28 (Claude Code)", "codex-cli 0.161.0"} {
		t.Run(output, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "native-version")
			if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s\\n' '"+output+"'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			err := verifyNativeVersion(t.Context(), harness.NativeClaude,
				path, dir, []string{testPathEnvironment}, time.Now().Add(time.Minute))
			if (err == nil) != (output == "2.1.27 (Claude Code)") {
				t.Fatalf("actual executable pin: %v", err)
			}
		})
	}
}
