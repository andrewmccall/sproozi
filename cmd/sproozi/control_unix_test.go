//go:build !windows

//nolint:goconst // Keep independent literal behavior expectations.
package main

import (
	"bytes"
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
	var stdout, stderr bytes.Buffer
	if code := cli([]string{"sessions", "--config", config}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "chmod 600 "+config) {
		t.Fatalf("exited %d: %q", code, stderr.String())
	}
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	stderr.Reset()
	if code := cli([]string{"init", "--directory", dir}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "chmod 700 "+dir) {
		t.Fatalf("exited %d: %q", code, stderr.String())
	}
}

func TestInitRejectsAnUnusableExistingConfiguration(t *testing.T) {
	dir := testutil.PrivateDir(t)
	file := filepath.Join(dir, "config.json")
	target := filepath.Join(dir, "elsewhere.json")
	if err := testutil.WriteFile(target, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		prepare func() error
		want    string
	}{
		{"shared", func() error {
			if err := testutil.WriteFile(file, []byte("{}"), 0600); err != nil {
				return err
			}
			return os.Chmod(file, 0644)
		}, "chmod 600 " + file},
		{"directory", func() error { return os.Mkdir(file, 0700) },
			"configuration " + file + " is a directory; it must be a regular file: move the directory aside"},
		{"symlink", func() error { return os.Symlink(target, file) },
			"configuration " + file + " is a symbolic link; it must be a regular file"},
		{"dangling", func() error { return os.Symlink(filepath.Join(dir, "absent"), file) },
			"configuration " + file + " is a symbolic link; it must be a regular file"},
	} {
		if err := os.RemoveAll(file); err != nil {
			t.Fatal(err)
		}
		if err := tc.prepare(); err != nil {
			t.Fatal(err)
		}
		var stdout, stderr bytes.Buffer
		if code := cli([]string{"init", "--directory", dir}, &stdout, &stderr); code != 1 ||
			!strings.Contains(stderr.String(), tc.want) || strings.Contains(stderr.String(), "chmod 700") {
			t.Fatalf("%s: exited %d: %q", tc.name, code, stderr.String())
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

// A missing socket is named as such, never repaired with chmod on its directory.
func TestMissingSocketIsNamedWithoutChmod(t *testing.T) {
	socket := "/sproozi-missing-" + uuid.NewString() + ".sock"
	var stdout, stderr bytes.Buffer
	code := cli([]string{"status", uuid.NewString(), "--socket", socket}, &stdout, &stderr)
	want := "Sproozi failed: docker (home): no home daemon is listening on the private control socket: " +
		socket + " does not exist; start home with \"sproozi serve\""
	if code != 1 || !strings.HasPrefix(stderr.String(), want) || strings.Contains(stderr.String(), "chmod") {
		t.Fatalf("exited %d: %q", code, stderr.String())
	}
	for _, path := range []string{"/", filepath.Join(testutil.PrivateDir(t), "absent")} {
		got := privateHint(&privatefs.UnsafePathError{Path: path}, "").Error()
		if strings.Contains(got, "chmod") || !strings.HasSuffix(got, "; use a private directory you own instead") {
			t.Errorf("%s: %q", path, got)
		}
	}
}

// An explicit --socket that is too long names the flag, not config.json.
func TestLongExplicitSocketNamesTheFlag(t *testing.T) {
	socket := "/" + strings.Repeat("s", 200) + ".sock"
	var stdout, stderr bytes.Buffer
	code := cli([]string{"status", uuid.NewString(), "--socket", socket}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "; use a shorter --socket path") ||
		strings.Contains(stderr.String(), `"directory"`) {
		t.Fatalf("exited %d: %q", code, stderr.String())
	}
}
