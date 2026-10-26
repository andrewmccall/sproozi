//nolint:goconst // Command lines in tests use literal flags to show the public interface.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImagesBuildRejectsAmbiguousRequests(t *testing.T) {
	for _, args := range [][]string{
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
	writeSourceTree(t, dir, nil)
	previous := build
	t.Cleanup(func() { build = previous })
	build.version = "0.2.0"
	if err := os.WriteFile(filepath.Join(dir, "release.json"), []byte(`{"version":"0.1.0"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	q := options{build: dir}
	if _, err := q.imageBuildArgs(dir); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched source accepted: %v", err)
	}
}

// sourceTreeFiles are the files imageBuildArgs requires in a source directory.
var sourceTreeFiles = []string{"go.mod", imageBuildRecipe, "Dockerfile", "hack/demo/codex.Dockerfile",
	"internal/runtime/docker/image/clients.Dockerfile", "internal/runtime/docker/image/Dockerfile",
	"hack/clients/install.sh"}

func writeSourceTree(t *testing.T, dir string, release []byte) {
	t.Helper()
	files := map[string][]byte{}
	for _, name := range sourceTreeFiles {
		files[name] = nil
	}
	if release != nil {
		files["release.json"] = release
	}
	for name, data := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// testRelease is the CLI version the release source tests pretend to be.
const testRelease = "0.2.0"

// sourceBundle builds a testRelease source bundle with the given extra entries.
func sourceBundle(t *testing.T, extra ...*tar.Header) []byte {
	version := testRelease
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	archive := tar.NewWriter(gz)
	top := "sproozi-" + version + "/"
	add := func(header *tar.Header, data []byte) {
		header.Size = int64(len(data))
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	add(&tar.Header{Name: top, Typeflag: tar.TypeDir, Mode: 0o755}, nil)
	for _, name := range sourceTreeFiles {
		add(&tar.Header{Name: top + name, Typeflag: tar.TypeReg, Mode: 0o644}, nil)
	}
	add(&tar.Header{Name: top + "release.json", Typeflag: tar.TypeReg, Mode: 0o644},
		[]byte(`{"version":"`+version+`"}`))
	for _, header := range extra {
		add(header, nil)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// serveRelease publishes a bundle and checksums.txt as release testRelease.
func serveRelease(t *testing.T, bundle []byte, checksum string) {
	version := testRelease
	t.Helper()
	name := sourceBundleName(version)
	if checksum == "" {
		sum := sha256.Sum256(bundle)
		checksum = hex.EncodeToString(sum[:])
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v"+version+"/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  other.tar.gz\n%s  %s\n", strings.Repeat("0", 64), checksum, name)
	})
	mux.HandleFunc("/v"+version+"/"+name, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(bundle) })
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	previous := releaseDownloads
	releaseDownloads = server.URL
	t.Cleanup(func() { releaseDownloads = previous })
}

func fakeDocker(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "docker.log")
	script := "#!/bin/sh\npwd > \"$BUILD_LOG\"\ncat release.json >> \"$BUILD_LOG\"\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BUILD_LOG", log)
	t.Setenv("SPROOZI_ROOT", filepath.Join(dir, "absent"))
	return log
}

func setVersion(t *testing.T, version string) {
	t.Helper()
	previous := build
	t.Cleanup(func() { build = previous })
	build.version = version
}

func TestImagesBuildFetchesReleaseSource(t *testing.T) {
	if isWindows() {
		t.Skip("fake Docker uses a POSIX shell")
	}
	setVersion(t, "0.2.0")
	serveRelease(t, sourceBundle(t), "")
	log := fakeDocker(t)
	var out, stderr bytes.Buffer
	if code := cli([]string{"images", "build"}, &out, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	built, release, _ := strings.Cut(string(data), "\n")
	if release != `{"version":"0.2.0"}` {
		t.Fatalf("build did not run in the release source: %q", data)
	}
	if _, err := os.Stat(built); !os.IsNotExist(err) {
		t.Fatalf("downloaded source %s was not removed: %v", built, err)
	}
}

func TestImagesBuildRejectsBadReleaseSource(t *testing.T) {
	if isWindows() {
		t.Skip("fake Docker uses a POSIX shell")
	}
	setVersion(t, "0.2.0")
	escape := &tar.Header{Name: "sproozi-0.2.0/../escape", Typeflag: tar.TypeReg, Mode: 0o644}
	link := &tar.Header{Name: "sproozi-0.2.0/link", Typeflag: tar.TypeSymlink, Linkname: "/etc"}
	for name, release := range map[string]struct {
		bundle   []byte
		checksum string
		want     string
	}{
		"checksum": {sourceBundle(t), strings.Repeat("a", 64), "checksum does not match"},
		"escape":   {sourceBundle(t, escape), "", "unsafe path"},
		"symlink":  {sourceBundle(t, link), "", "unsupported entry"},
	} {
		t.Run(name, func(t *testing.T) {
			serveRelease(t, release.bundle, release.checksum)
			log := fakeDocker(t)
			var out, stderr bytes.Buffer
			if code := cli([]string{"images", "build"}, &out, &stderr); code != 1 ||
				!strings.Contains(stderr.String(), release.want) {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			if _, err := os.Stat(log); !os.IsNotExist(err) {
				t.Fatal("Docker ran with a rejected source")
			}
		})
	}
}

func TestImagesBuildNeedsSourceForDevelopmentBuilds(t *testing.T) {
	setVersion(t, sourceVersion)
	var out, stderr bytes.Buffer
	if code := cli([]string{"images", "build"}, &out, &stderr); code != 2 ||
		!strings.Contains(stderr.String(), "--build") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
}

func TestInstalledSourceFollowsPackageLayout(t *testing.T) {
	if isWindows() {
		t.Skip("Homebrew layout uses symbolic links")
	}
	setVersion(t, "0.2.0")
	// Homebrew links PREFIX/bin/sproozi to Cellar/sproozi/VERSION/bin/sproozi
	// and installs the release source in Cellar/sproozi/VERSION/share/sproozi.
	root := t.TempDir()
	keg := filepath.Join(root, "Cellar", "sproozi", "0.2.0")
	exe := filepath.Join(keg, "bin", "sproozi")
	writeSourceTree(t, filepath.Join(keg, "share", "sproozi"), []byte(`{"version":"0.2.0"}`))
	if err := os.MkdirAll(filepath.Dir(exe), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "bin", "sproozi")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(exe, link); err != nil {
		t.Fatal(err)
	}
	dir, ok := installedSource(link)
	if want, _ := filepath.EvalSymlinks(filepath.Join(keg, "share", "sproozi")); !ok || dir != want {
		t.Fatalf("installed source = %q, %v", dir, ok)
	}
	setVersion(t, "0.3.0")
	if _, ok := installedSource(link); ok {
		t.Fatal("installed source for another version was used")
	}
}
