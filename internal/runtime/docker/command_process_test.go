package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/privatefs"
)

type helperInvocation struct {
	Args []string `json:"args"`
	Env  []string `json:"env"`
}

func buildDockerCommandHelper(t *testing.T, osType, logPath string) string {
	t.Helper()
	dir := t.TempDir()
	source := filepath.Join(dir, "main.go")
	program := fmt.Sprintf(`package main
import (
    "encoding/json"
    "fmt"
    "os"
    "strings"
)
func main() {
    args := os.Args[1:]
    payload := args
    if len(args) >= 4 && args[0] == "--host" && args[2] == "--config" { payload = args[4:] }
    f, _ := os.OpenFile(%q, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
    if f != nil { _, _ = fmt.Fprintln(f, strings.Join(payload, " ")); _ = f.Close() }
    if len(payload) > 0 && payload[0] == "info" { fmt.Print(%q); return }
    if len(payload) >= 3 && payload[0] == "container" && payload[1] == "inspect" {
        _ = json.NewEncoder(os.Stdout).Encode([]any{map[string]any{
            "Id": payload[2], "Config": map[string]any{"Labels": map[string]string{"sproozi.com/execution": "fixture-execution"}},
            "State": map[string]any{"Running": true, "Status": "running", "Pid": 42},
        }})
        return
    }
    _ = json.NewEncoder(os.Stdout).Encode(map[string]any{"args": args, "env": os.Environ()})
}
`, logPath, osType)
	if err := os.WriteFile(source, []byte(program), 0600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "docker-helper")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	cmd := exec.Command("go", "build", "-o", binary, source)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build Docker helper: %v: %s", err, output)
	}
	return binary
}

func privateDockerConfig(t *testing.T) string {
	t.Helper()
	dir, err := privatefs.TempDir("", "sproozi-docker-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func decodeHelperInvocation(t *testing.T, data []byte) helperInvocation {
	t.Helper()
	var invocation helperInvocation
	if err := json.Unmarshal(data, &invocation); err != nil {
		t.Fatalf("decode helper invocation: %v: %s", err, data)
	}
	return invocation
}

func environmentMap(values []string) map[string]string {
	result := make(map[string]string, len(values))
	for _, value := range values {
		name, content, ok := strings.Cut(value, "=")
		if ok {
			result[strings.ToUpper(name)] = content
		}
	}
	return result
}

func TestDockerCommandsUseFixedEndpointAndSanitizedEnvironment(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	engine, err := NewEngine(buildDockerCommandHelper(t, "linux", logPath), fixtureDockerEndpoint(t), privateDockerConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_CONFIG", "DOCKER_CERT_PATH", "DOCKER_TLS_VERIFY"} {
		t.Setenv(key, "hostile")
	}
	data, err := engine.commandInput(t.Context(), nil, 64<<10, "version")
	if err != nil {
		t.Fatal(err)
	}
	invocation := decodeHelperInvocation(t, data)
	wantPrefix := []string{"--host", engine.endpoint, "--config", engine.config}
	if !slices.Equal(invocation.Args, append(wantPrefix, "version")) {
		t.Fatalf("normal argv=%q", invocation.Args)
	}
	env := environmentMap(invocation.Env)
	for name := range env {
		if strings.HasPrefix(name, "DOCKER_") {
			t.Fatalf("inherited Docker environment %s", name)
		}
	}
	if env["HOME"] != engine.config || env["PATH"] == "" {
		t.Fatalf("sanitized environment=%#v", env)
	}
	assertDockerPlatformEnvironment(t, env, engine.config)

	containerID := strings.Repeat("a", 64)
	var attached bytes.Buffer
	if err = engine.AttachNative(context.Background(), Instance{ID: containerID, Execution: "fixture-execution"}, strings.NewReader("input"), &attached); err != nil {
		t.Fatal(err)
	}
	attach := decodeHelperInvocation(t, attached.Bytes())
	wantAttach := append(wantPrefix, execCommand, "--interactive", containerID, nativeCommandPath, "attach")
	if !slices.Equal(attach.Args, wantAttach) {
		t.Fatalf("attach argv=%q", attach.Args)
	}
	attachEnv := environmentMap(attach.Env)
	if attachEnv["HOME"] != engine.config {
		t.Fatalf("attach environment=%#v", attachEnv)
	}
	for name := range attachEnv {
		if strings.HasPrefix(name, "DOCKER_") {
			t.Fatalf("attach inherited Docker environment %s", name)
		}
	}
}

func TestDockerChecksLinuxBeforeFirstMutation(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "commands.log")
	engine, err := NewEngine(buildDockerCommandHelper(t, "windows", logPath), fixtureDockerEndpoint(t), privateDockerConfig(t))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("NewEngine invoked Docker: %v", err)
	}
	if _, err = engine.commandInput(t.Context(), nil, 4096, createCommand, "fixture"); err == nil || !strings.Contains(err.Error(), "Linux containers") {
		t.Fatalf("non-Linux mutation error=%v", err)
	}
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(logData)); got != "info --format {{.OSType}}" {
		t.Fatalf("commands before denial=%q", got)
	}
}
