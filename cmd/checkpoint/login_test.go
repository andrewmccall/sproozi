package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareLoginCreatesAnEmptyPrivateFileOnce(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "login")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".credentials.json")
	if err := prepareLogin(path, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() != 0 || info.Mode().Perm() != 0o600 {
		t.Fatalf("login file: %v %v", info, err)
	}
	// A later launch keeps the harness's sign-in untouched.
	if err = os.WriteFile(path, []byte(`{"signedIn":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err = prepareLogin(path, os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != `{"signedIn":true}` {
		t.Fatalf("existing sign-in changed: %s", data)
	}
}

func TestPrepareLoginRefusesALinkOrAWiderMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "login")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".credentials.json")
	if err := os.Symlink("/etc/passwd", path); err != nil {
		t.Fatal(err)
	}
	if err := prepareLogin(path, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("a link was accepted as the login file")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := prepareLogin(path, os.Getuid(), os.Getgid()); err == nil {
		t.Fatal("a readable login file was accepted")
	}
}

func TestInitLoginOnlyWritesInsideTheLoginMount(t *testing.T) {
	for _, path := range []string{"/workspace/.credentials.json", "/login/nested/.credentials.json", "/login"} {
		if err := initLogin(path, 1000, 1000); err == nil {
			t.Fatalf("init-login accepted %s", path)
		}
	}
}

func TestPrepareLoginKeepsTheDirectoryAndTightensItsMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "login")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := prepareLogin(filepath.Join(dir, ".credentials.json"), os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("login directory: %v %v", info, err)
	}
}
