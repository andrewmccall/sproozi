package localsetup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/andrewmccall/sproozi/internal/privatefs"
)

// Docker runs the Docker CLI against one local engine with a private CLI
// configuration directory. It never reads the user's registry credentials.
type Docker struct {
	Binary string `json:"binary"`
	Socket string `json:"socket"`
	// Config is the private CLI directory; it is not persisted in records.
	Config string `json:"-"`
}

// Failure names the component that failed so diagnostics can separate Docker,
// registry, network, TLS and provider problems.
type Failure struct {
	Component string
	Err       error
	Next      string
}

func (f *Failure) Error() string {
	if f.Next == "" {
		return fmt.Sprintf("%s: %v", f.Component, f.Err)
	}
	return fmt.Sprintf("%s: %v; next: %s", f.Component, f.Err, f.Next)
}

func (f *Failure) Unwrap() error { return f.Err }

func fail(component string, err error, next string) error {
	return &Failure{Component: component, Err: err, Next: next}
}

// DiscoverDocker locates the docker executable and the current context's local
// Unix socket, then checks the engine runs Linux containers. Remote TCP
// engines are outside automatic setup.
func DiscoverDocker(ctx context.Context, privateConfig string) (Docker, error) {
	binary, err := exec.LookPath("docker")
	if err == nil {
		binary, err = filepath.Abs(binary)
	}
	if err != nil {
		return Docker{}, fail("docker", errors.New("docker executable not found on PATH"), "install and start Docker Desktop or Docker Engine")
	}
	// Reading the current context is the only use of the user's CLI settings.
	cmd := exec.CommandContext(ctx, binary, "context", "inspect", "--format", `{{(index .Endpoints "docker").Host}}`)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return Docker{}, fail("docker", fmt.Errorf("current Docker context is unreadable: %w", err), "run docker context ls")
	}
	host := strings.TrimSpace(string(out))
	if env := os.Getenv("DOCKER_HOST"); env != "" {
		host = env
	}
	socket, ok := strings.CutPrefix(host, "unix://")
	if !ok || !filepath.IsAbs(socket) {
		return Docker{}, fail("docker", fmt.Errorf("engine endpoint %q is not a local Unix socket", host), "select a local Docker context")
	}
	d := Docker{Binary: binary, Socket: socket, Config: privateConfig}
	if err = d.prepareConfig(); err != nil {
		return Docker{}, err
	}
	osType, err := d.Output(ctx, nil, "info", "--format", "{{.OSType}}")
	if err != nil {
		return Docker{}, fail("docker", fmt.Errorf("engine unavailable: %w", err), "start Docker and retry")
	}
	if strings.TrimSpace(osType) != "linux" {
		return Docker{}, fail("docker", errors.New("engine does not run Linux containers"), "switch Docker to Linux containers")
	}
	return d, nil
}

// prepareConfig creates the private CLI directory. It names the user's CLI
// plugin directory so BuildKit is available without the user's credentials.
func (d Docker) prepareConfig() error {
	if err := privatefs.EnsureDir(d.Config); err != nil {
		return err
	}
	settings := map[string]any{}
	if home, err := os.UserHomeDir(); err == nil {
		plugins := filepath.Join(home, ".docker", "cli-plugins")
		if info, err := os.Stat(plugins); err == nil && info.IsDir() {
			settings["cliPluginsExtraDirs"] = []string{plugins}
		}
	}
	data, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	current, err := privatefs.ReadFile(filepath.Join(d.Config, "config.json"), 64<<10)
	if err == nil && bytes.Equal(current, data) {
		return nil
	}
	return privatefs.Replace(d.Config, "config.json", data)
}

// Output runs one Docker CLI command and returns bounded stdout.
func (d Docker) Output(ctx context.Context, stdin io.Reader, args ...string) (string, error) {
	var stdout, stderr bytes.Buffer
	err := d.Stream(ctx, stdin, &limited{w: &stdout, n: 4 << 20}, &limited{w: &stderr, n: 16 << 10}, args...)
	if err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return stdout.String(), errors.New(message)
	}
	return stdout.String(), nil
}

// Stream runs one Docker CLI command with caller-owned output.
func (d Docker) Stream(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, args ...string) error {
	full := append([]string{"--host", "unix://" + d.Socket, "--config", d.Config}, args...)
	cmd := exec.CommandContext(ctx, d.Binary, full...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + d.Config, "LC_ALL=C", "DOCKER_BUILDKIT=1"}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	return cmd.Run()
}

type limited struct {
	w io.Writer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if len(p) > l.n {
		return 0, errors.New("docker output exceeds bound")
	}
	l.n -= len(p)
	return l.w.Write(p)
}
