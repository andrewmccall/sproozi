package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/testutil"

	"github.com/andrewmccall/sproozi/internal/privatefs"
)

func TestHelpAndVersionNeedNoHome(t *testing.T) {
	t.Setenv("SPROOZI_CONFIG", "does-not-exist")
	for _, arg := range []string{"--help", "-h", "help", "version", "--version"} {
		output := captureCLIOutput(t, []string{arg})
		if strings.Contains(arg, "version") {
			if output != "sproozi dev (commit unknown, built unknown)\n" {
				t.Fatalf("%s: %q", arg, output)
			}
		} else if !strings.Contains(output, "Usage: sproozi COMMAND [flags]\n") {
			t.Fatalf("%s: missing usage in %q", arg, output)
		}
	}
}

func captureCLIOutput(t *testing.T, args []string) string {
	t.Helper()
	var output bytes.Buffer
	if err := run(args, &output); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

func TestInitCreatesPrivateDirectoryAndPreservesFiles(t *testing.T) {
	path := filepath.Join(testutil.PrivateDir(t), "configuration")
	output := captureCLIOutput(t, []string{actionInit, "--directory", path})
	if strings.TrimSpace(output) != path {
		t.Fatalf("unexpected directory: %q", output)
	}
	if err := privatefs.Check(path, true); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(path, "config.json")
	if err := testutil.WriteFile(file, []byte("keep this"), 0600); err != nil {
		t.Fatal(err)
	}
	captureCLIOutput(t, []string{actionInit, "--directory", path})
	data, err := os.ReadFile(file)
	if err != nil || string(data) != "keep this" {
		t.Fatalf("init changed the existing configuration: %q, %v", data, err)
	}
}
