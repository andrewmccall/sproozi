//go:build !windows

//nolint:goconst // Keep independent literal behavior expectations.
package main

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/testutil"
	"github.com/google/uuid"
)

func listenTestControl(address string) (net.Listener, error) {
	listener, err := net.Listen("unix", address)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(address, 0600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

func TestUnsafeConfigurationNamesThePermissionRepair(t *testing.T) {
	dir := testutil.PrivateDir(t)
	config := filepath.Join(dir, "config.json")
	if err := testutil.WriteFile(config, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(config, 0644); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runCLI("sessions", "--config", config); code != 1 ||
		!strings.Contains(stderr, "chmod 600 "+config) {
		t.Fatalf("exited %d: %q", code, stderr)
	}
	for _, tc := range []struct {
		name    string
		prepare func() error
		want    string
	}{
		{"directory", func() error { return os.Mkdir(config, 0700) }, "configuration " + config + " is a directory"},
		{"symlink", func() error { return os.Symlink(filepath.Join(dir, "absent"), config) },
			"configuration " + config + " is a symbolic link"},
	} {
		if err := os.RemoveAll(config); err != nil {
			t.Fatal(err)
		}
		if err := tc.prepare(); err != nil {
			t.Fatal(err)
		}
		if code, _, stderr := runCLI("status", "abcd", "--config", config); code != 1 ||
			!strings.Contains(stderr, tc.want) || strings.Contains(stderr, "chmod") {
			t.Fatalf("%s: exited %d: %q", tc.name, code, stderr)
		}
	}
}

func TestPrintedPathsAreShellQuoted(t *testing.T) {
	for path, want := range map[string]string{
		"/plain/path.json":                       "/plain/path.json",
		"/Users/a/Library/Application Support/x": "'/Users/a/Library/Application Support/x'",
		"/it's":                                  `'/it'\''s'`,
	} {
		if got := shellQuote(path); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", path, got, want)
		}
	}
}

// A missing socket says the service is not running, never suggesting chmod.
func TestMissingSocketIsNamedWithoutChmod(t *testing.T) {
	socket := "/sproozi-missing-" + uuid.NewString() + ".sock"
	code, _, stderr := runCLI("status", uuid.NewString(), "--socket", socket)
	if code != 1 || stderr != "error: the Sproozi service is not running; start it with \"sproozi service start\"\n" {
		t.Fatalf("exited %d: %q", code, stderr)
	}
	for _, path := range []string{"/", filepath.Join(testutil.PrivateDir(t), "absent")} {
		got := explain(&privatefs.UnsafePathError{Path: path})
		if strings.Contains(got, "chmod") || !strings.HasSuffix(got, "; use a private directory you own instead") {
			t.Errorf("%s: %q", path, got)
		}
	}
}

// A --socket that cannot work names the flag.
func TestUnusableSocketNamesTheFlag(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "plain")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for want, socket := range map[string]string{
		"; use a shorter --socket path":                                 "/" + strings.Repeat("s", 200) + ".sock",
		"--socket " + shellQuote(dir) + " is a directory, not a socket": dir,
		"--socket " + file + " is a regular file, not a socket":         file,
	} {
		if code, _, stderr := runCLI("status", uuid.NewString(), "--socket", socket); code != 2 ||
			!strings.Contains(stderr, want) {
			t.Errorf("%s: exited %d: %q", socket, code, stderr)
		}
	}
}
