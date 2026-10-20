// Package docker owns local Docker execution resources. Shared conversation and
// checkpoint code does not import Docker commands or engine types.
package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/andrewmccall/sproozi/internal/privatefs"
)

var objectID = regexp.MustCompile(`^[0-9a-f]{64}$`)
var resourceName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

const createdStatus = "created"
const containerCommand = "container"

const ownerLabel = "sproozi.com/execution"

// Engine speaks only to the administrator-selected local engine endpoint, with
// no inherited Docker contexts, CLI plugins or credential-helper configuration.
type Engine struct {
	binary     string
	endpoint   string
	config     string
	execute    func(context.Context, []byte, int, []string) ([]byte, error)
	linuxMu    sync.Mutex
	linuxReady bool
}

func NewEngine(binary, socket, privateConfig string) (*Engine, error) {
	if !filepath.IsAbs(binary) || !filepath.IsAbs(privateConfig) {
		return nil, errors.New("docker requires absolute executable and private configuration paths")
	}
	endpoint, err := localDockerEndpoint(socket)
	if err != nil {
		return nil, err
	}
	if err = privatefs.Check(privateConfig, true); err != nil {
		return nil, errors.New("docker configuration must be an owned private directory")
	}
	return &Engine{binary: binary, endpoint: endpoint, config: privateConfig}, nil
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

// NetworkExists reports whether the engine has a network with exactly this name.
func (e *Engine) NetworkExists(ctx context.Context, name string) (bool, error) {
	if !resourceName.MatchString(name) {
		return false, errors.New("docker network name is invalid")
	}
	data, err := e.command(ctx, "network", "ls", "--filter", "name=^"+name+"$", "--format", "{{.Name}}")
	if err != nil {
		return false, err
	}
	for line := range strings.SplitSeq(string(data), "\n") {
		if strings.TrimSpace(line) == name {
			return true, nil
		}
	}
	return false, nil
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
	data, err := e.command(ctx, containerCommand, "inspect", instance.ID)
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
	_, err = e.command(ctx, containerCommand, "start", instance.ID)
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
		if _, err = e.command(ctx, containerCommand, "stop", "--time", "10", instance.ID); err != nil {
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
	_, err = e.command(ctx, containerCommand, "rm", instance.ID)
	return err
}

// removeManaged stops and removes the exact incarnation registered under name.
// It is used only after the placement has durably recorded that it is removing
// its own resources: an absent name, or one now held by a different
// container, then means this incarnation is already gone.
func (e *Engine) removeManaged(ctx context.Context, instance Instance, name string) error {
	current, found, err := e.lookup(ctx, name, instance.Execution)
	if err != nil || !found || current.ID != instance.ID {
		return err
	}
	if err = e.Stop(ctx, instance); err != nil {
		return err
	}
	return e.RemoveStopped(ctx, instance)
}

// RemoveVolume removes the exact retained tmpfs volume. An absent volume is
// already removed; a replacement with different identity is never removed.
func (e *Engine) RemoveVolume(ctx context.Context, v Volume) error {
	if !resourceName.MatchString(v.Name) {
		return errors.New("invalid Docker tmpfs volume")
	}
	data, err := e.command(ctx, volumeCommand, "ls", "--quiet", "--filter", "name="+v.Name)
	if err != nil {
		return err
	}
	if !slices.Contains(strings.Fields(string(data)), v.Name) {
		return nil
	}
	if _, err = e.volume(ctx, v); err != nil {
		return err
	}
	_, err = e.command(ctx, volumeCommand, "rm", v.Name)
	return err
}

// boundedOutput keeps at most limit bytes of a command's output. It does not
// embed bytes.Buffer: io.Copy would then use Buffer.ReadFrom and bypass Write.
// Output past the limit fails, unless truncate keeps the first limit bytes
// for an error message and discards the rest.
type boundedOutput struct {
	buffer              bytes.Buffer
	limit               int
	truncate, truncated bool
}

func (b *boundedOutput) Write(data []byte) (int, error) {
	room := b.limit - b.buffer.Len()
	if len(data) <= room {
		return b.buffer.Write(data)
	}
	if !b.truncate {
		return 0, errors.New("docker control response exceeds limit")
	}
	b.truncated = true
	_, _ = b.buffer.Write(data[:max(room, 0)])
	return len(data), nil
}

func (b *boundedOutput) Bytes() []byte { return b.buffer.Bytes() }

// String returns the kept output, marked when the rest was discarded.
func (b *boundedOutput) String() string {
	if b.truncated {
		return b.buffer.String() + " [output truncated]"
	}
	return b.buffer.String()
}

func (e *Engine) command(ctx context.Context, args ...string) ([]byte, error) {
	return e.commandInput(ctx, nil, 128<<10, args...)
}
