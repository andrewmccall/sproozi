// Package docker owns local Docker execution resources. Shared conversation and
// checkpoint code does not import Docker commands or engine types.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var objectID = regexp.MustCompile(`^[0-9a-f]{64}$`)
var resourceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

const createdStatus = "created"

const ownerLabel = "sproozi.com/execution"

// Engine speaks only to the administrator-selected local Unix socket, with no
// inherited Docker contexts, CLI plugins or credential-helper configuration.
type Engine struct {
	binary  string
	socket  string
	config  string
	execute func(context.Context, []byte, int, []string) ([]byte, error)
}

func NewEngine(binary, socket, privateConfig string) (*Engine, error) {
	if !filepath.IsAbs(binary) || !filepath.IsAbs(socket) || !filepath.IsAbs(privateConfig) {
		return nil, errors.New("docker requires absolute executable, local socket and private configuration paths")
	}
	info, err := os.Lstat(privateConfig)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("docker configuration must be an owned private directory")
	}
	return &Engine{binary: binary, socket: socket, config: privateConfig}, nil
}

// Instance selects an immutable container incarnation, never a reusable name.
// The execution label binds operations to the placement service's retained ID.
type Instance struct {
	ID        string
	Execution string
}

type Observation struct {
	Instance
	Running bool
	Status  string
	PID     int
}

// ImageID resolves an administrator-selected image reference once. Container
// creation must use its returned immutable content ID, not a mutable tag.
func (e *Engine) ImageID(ctx context.Context, reference string) (string, error) {
	if reference == "" || strings.HasPrefix(reference, "-") {
		return "", errors.New("docker image reference is required")
	}
	data, err := e.command(ctx, "image", "inspect", "--format", "{{.Id}}", reference)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(data))
	if !strings.HasPrefix(id, "sha256:") || !objectID.MatchString(strings.TrimPrefix(id, "sha256:")) {
		return "", errors.New("docker returned an invalid immutable image identity")
	}
	return id, nil
}

// CreateIsolated creates an inert, network-disabled execution. This primitive
// does not supply workspace storage, gateway routing or portable-session launch.
// The managed placement must establish those before any native activation.
func (e *Engine) CreateIsolated(ctx context.Context, execution, imageID string, command []string, memoryBytes, cpuMilli, pids int64) (Instance, error) {
	if !resourceName.MatchString(execution) || !strings.HasPrefix(imageID, "sha256:") ||
		!objectID.MatchString(strings.TrimPrefix(imageID, "sha256:")) || len(command) == 0 ||
		memoryBytes < 16<<20 || memoryBytes > 16<<30 || cpuMilli < 1 || cpuMilli > 16000 || pids < 1 || pids > 1024 {
		return Instance{}, errors.New("invalid isolated Docker execution configuration")
	}
	args := make([]string, 0, 30+len(command))
	args = append(args, createCommand, labelFlag, ownerLabel+"="+execution,
		"--network", "none", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges",
		"--user", nativeUser, "--pids-limit", strconv.FormatInt(pids, 10),
		"--memory", strconv.FormatInt(memoryBytes, 10), "--memory-swap", strconv.FormatInt(memoryBytes, 10),
		"--cpus", strconv.FormatFloat(float64(cpuMilli)/1000, 'f', 3, 64),
		"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=16777216,uid=1000,gid=1000,mode=700",
		imageID)
	args = append(args, command...)
	data, err := e.command(ctx, args...)
	if err != nil {
		return Instance{}, err
	}
	id := strings.TrimSpace(string(data))
	if !objectID.MatchString(id) {
		return Instance{}, errors.New("docker returned an invalid container incarnation")
	}
	return Instance{ID: id, Execution: execution}, nil
}

func (e *Engine) Observe(ctx context.Context, instance Instance) (Observation, error) {
	if !objectID.MatchString(instance.ID) || !resourceName.MatchString(instance.Execution) {
		return Observation{}, errors.New("invalid Docker incarnation")
	}
	data, err := e.command(ctx, "container", "inspect", instance.ID)
	if err != nil {
		return Observation{}, err
	}
	var records []struct {
		ID     string                             `json:"Id"`
		Config struct{ Labels map[string]string } `json:"Config"`
		State  struct {
			Running bool
			Status  string
			PID     int `json:"Pid"`
		}
	}
	if err = json.Unmarshal(data, &records); err != nil || len(records) != 1 ||
		records[0].ID != instance.ID || records[0].Config.Labels[ownerLabel] != instance.Execution {
		return Observation{}, errors.New("docker incarnation or execution binding is unavailable")
	}
	s := records[0].State
	return Observation{Instance: instance, Running: s.Running, Status: s.Status, PID: s.PID}, nil
}

func (e *Engine) Start(ctx context.Context, instance Instance) error {
	before, err := e.Observe(ctx, instance)
	if err != nil {
		return err
	}
	if before.Running {
		return nil
	}
	// An exited native placement must never be reopened. Each resumed placement
	// creates a fresh container with fresh authority.
	if before.Status != createdStatus || before.PID != 0 {
		return errors.New("docker execution cannot be reopened")
	}
	_, err = e.command(ctx, "container", "start", instance.ID)
	return err
}

// Stop observes the actual container after engine termination. An unavailable
// engine or ambiguous state is an error, never evidence that writers stopped.
func (e *Engine) Stop(ctx context.Context, instance Instance) error {
	before, err := e.Observe(ctx, instance)
	if err != nil {
		return err
	}
	if before.Running {
		if _, err = e.command(ctx, "container", "stop", "--time", "10", instance.ID); err != nil {
			return err
		}
	}
	after, err := e.Observe(ctx, instance)
	if err != nil {
		return err
	}
	if after.Running || after.PID != 0 || (after.Status != exitedStatus && after.Status != createdStatus) {
		return errors.New("docker container stop was not confirmed")
	}
	return nil
}

func (e *Engine) RemoveStopped(ctx context.Context, instance Instance) error {
	state, err := e.Observe(ctx, instance)
	if err != nil {
		return err
	}
	if state.Running || state.PID != 0 || (state.Status != exitedStatus && state.Status != createdStatus) {
		return errors.New("docker container must be stopped before removal")
	}
	_, err = e.command(ctx, "container", "rm", instance.ID)
	return err
}

type boundedOutput struct {
	bytes.Buffer
	limit int
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	if len(data) > b.limit-b.Len() {
		return 0, errors.New("docker control response exceeds limit")
	}
	return b.Buffer.Write(data)
}

func (e *Engine) command(ctx context.Context, args ...string) ([]byte, error) {
	return e.commandInput(ctx, nil, 128<<10, args...)
}
