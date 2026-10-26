//nolint:goconst // Command lines in tests use literal flags to show the public interface.
package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImagesBuildRejectsAmbiguousRequests(t *testing.T) {
	for _, args := range [][]string{
		{"images", "build"},
		{"images", "build", "--build", ".", "--push"},
		{"images", "build", "--build", ".", "--platform", "linux/amd64,linux/arm64"},
		{"images", "build", "--build", ".", "--platform", "darwin/arm64"},
		{"images", "build", "--build", ".", "--image", "sha256:abc"},
		{"images", "prepare", "--push"},
		{"init", "--image", "sha256:abc", "--build", "."},
	} {
		var out, stderr bytes.Buffer
		if code := cli(args, &out, &stderr); code != 2 {
			t.Errorf("%v: exit %d, %s", args, code, stderr.String())
		}
	}
}

func TestImagesBuildWithoutInstallation(t *testing.T) {
	if isWindows() {
		t.Skip("fake Docker uses a POSIX shell")
	}
	dir := t.TempDir()
	log := filepath.Join(dir, "docker.log")
	// Capture the real CLI invocation, including its working directory. The
	// source tree is the repository; no installation or credentials exist.
	script := "#!/bin/sh\npwd > \"$BUILD_LOG\"\nprintf '%s\\n' \"$@\" >> \"$BUILD_LOG\"\nexit \"${BUILD_EXIT:-0}\"\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BUILD_LOG", log)
	t.Setenv("SPROOZI_ROOT", filepath.Join(dir, "absent"))
	source, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	jsonArg := "--json"
	for _, push := range []bool{false, true} {
		args := []string{"images", "build", "--build", source, "--image-prefix", "localhost:5000/test",
			"--image-tag", "custom", jsonArg}
		mode := "--load"
		if push {
			args = append(args, "--push", "--platform", "linux/amd64,linux/arm64")
			mode = "--push"
		}
		var out, stderr bytes.Buffer
		if code := cli(args, &out, &stderr); code != 0 {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		data, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(data), source+"\n") || !strings.Contains(string(data), mode+"\nnative\ncontrol\n") {
			t.Fatalf("unexpected invocation: %s", data)
		}
		var result struct {
			Native string
			Pushed bool
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil ||
			result.Native != "localhost:5000/test/sproozi-native:custom" || result.Pushed != push {
			t.Fatalf("unexpected result: %s (%v)", out.String(), err)
		}
	}
	t.Setenv("BUILD_EXIT", "1")
	var out, stderr bytes.Buffer
	if code := cli([]string{"images", "build", "--build", source, jsonArg}, &out, &stderr); code != 1 || out.Len() != 0 {
		t.Fatalf("failed build reported success: exit=%d out=%s stderr=%s", code, out.String(), stderr.String())
	}
}

func TestImagesBuildRejectsMismatchedRelease(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"go.mod", imageBuildRecipe, "Dockerfile", "hack/demo/codex.Dockerfile",
		"internal/runtime/docker/image/clients.Dockerfile", "internal/runtime/docker/image/Dockerfile",
		"hack/clients/install.sh"} {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	previous := build
	t.Cleanup(func() { build = previous })
	build.version = "0.2.0"
	if err := os.WriteFile(filepath.Join(dir, "release.json"), []byte(`{"version":"0.1.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	q := options{build: dir}
	if _, err := q.imageBuildArgs(); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched source accepted: %v", err)
	}
}
