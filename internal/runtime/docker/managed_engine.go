package docker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"strings"

	"github.com/andrewmccall/sproozi/internal/harness"

	"github.com/andrewmccall/sproozi/internal/workspace"
)

const (
	quiesceCommand    = "quiesce"
	harnessFlag       = "--harness"
	exitedStatus      = "exited"
	localVolumeDriver = "local"
	labelFlag         = "--label"
	volumeCommand     = "volume"
	maxBytesFlag      = "--max-bytes"
	execCommand       = "exec"
	createCommand     = "create"
	prepareCommand    = "prepare"
	allCapabilities   = "ALL"
	canonicalNetAdmin = "CAP_NET_ADMIN"
)

// Volume identifies the observed local-driver tmpfs incarnation. Docker volumes
// have no immutable UID: creation time, owner label and exact options are retained
// and replacement or option changes fail closed.
type Volume struct {
	Name      string `json:"name"`
	Execution string `json:"execution"`
	CreatedAt string `json:"createdAt"`
	Bytes     int64  `json:"bytes"`
}

func volumeOptions(size int64) string {
	return fmt.Sprintf("size=%d,uid=1000,gid=1000,mode=0700,nosuid,nodev", size)
}
func (e *Engine) EnsureVolume(ctx context.Context, name, execution string, size int64) (Volume, error) {
	if !resourceName.MatchString(name) || !resourceName.MatchString(execution) || size < 16<<20 || size > 4<<30 {
		return Volume{}, errors.New("invalid Docker tmpfs volume")
	}
	_, err := e.command(ctx, volumeCommand, createCommand, "--name", name, "--driver", localVolumeDriver, labelFlag, ownerLabel+"="+execution,
		"--opt", "type=tmpfs", "--opt", "device=tmpfs", "--opt", "o="+volumeOptions(size))
	if err != nil {
		return Volume{}, err
	}
	return e.volume(ctx, Volume{Name: name, Execution: execution, Bytes: size})
}
func (e *Engine) volume(ctx context.Context, v Volume) (Volume, error) {
	data, err := e.command(ctx, volumeCommand, "inspect", v.Name)
	if err != nil {
		return Volume{}, err
	}
	var rows []struct {
		Name, Driver, CreatedAt string
		Labels                  map[string]string
		Options                 map[string]string
	}
	if json.Unmarshal(data, &rows) != nil || len(rows) != 1 {
		return Volume{}, errors.New("docker volume observation is unavailable")
	}
	r := rows[0]
	if r.Name != v.Name || r.Driver != localVolumeDriver || r.CreatedAt == "" || r.Labels[ownerLabel] != v.Execution || r.Options["type"] != "tmpfs" || r.Options["device"] != "tmpfs" || r.Options["o"] != volumeOptions(v.Bytes) || len(r.Options) != 3 || (v.CreatedAt != "" && r.CreatedAt != v.CreatedAt) {
		return Volume{}, errors.New("docker volume identity or quota changed")
	}
	v.CreatedAt = r.CreatedAt
	return v, nil
}

type ManagedSpec struct {
	Name, Execution, ImageID, Role, NetworkMode string
	IntentDigest                                string
	Volume                                      *Volume
	MemoryBytes, CPUMilli, PIDs                 int64
	Deadline                                    string
	GatewayIPv4                                 string
	GatewayPort                                 uint16
}

func (s ManagedSpec) command() ([]string, error) {
	switch s.Role {
	case roleAgent:
		return []string{nativeCommandPath, "serve", "--bootstrap", "--deadline", s.Deadline}, nil
	case roleHelper:
		return []string{checkpointCommandPath, "hold", "--deadline", s.Deadline}, nil
	case roleAnchor:
		ip, err := netip.ParseAddr(s.GatewayIPv4)
		if err != nil || !ip.Is4() || ip.IsUnspecified() || ip.IsMulticast() || s.GatewayPort == 0 {
			return nil, errors.New("invalid fixed local gateway")
		}
		return []string{networkCommandPath, ip.String(), strconv.Itoa(int(s.GatewayPort))}, nil
	default:
		return nil, errors.New("invalid managed Docker role")
	}
}
func (e *Engine) EnsureManaged(ctx context.Context, s ManagedSpec) (Instance, error) {
	if !resourceName.MatchString(s.Name) || !resourceName.MatchString(s.Execution) || !strings.HasPrefix(s.ImageID, "sha256:") || !objectID.MatchString(strings.TrimPrefix(s.ImageID, "sha256:")) || s.MemoryBytes < 16<<20 || s.MemoryBytes > 16<<30 || s.CPUMilli < 1 || s.CPUMilli > 16000 || s.PIDs < 1 || s.PIDs > 1024 {
		return Instance{}, errors.New("invalid managed Docker limits")
	}
	cmd, err := s.command()
	if err != nil {
		return Instance{}, err
	}
	// Named lookup resolves a lost create acknowledgement, never a missing retained
	// incarnation. Exact command/security/mount/network checks precede adoption.
	instance, found, err := e.lookup(ctx, s.Name, s.Execution)
	if err != nil {
		return Instance{}, err
	}
	if !found {
		args := []string{createCommand, "--name", s.Name, labelFlag, ownerLabel + "=" + s.Execution,
			labelFlag, "sproozi.com/launch=" + s.IntentDigest,
			"--network", s.NetworkMode, "--read-only", "--cap-drop", allCapabilities, "--security-opt", "no-new-privileges",
			"--pids-limit", strconv.FormatInt(s.PIDs, 10), "--memory", strconv.FormatInt(s.MemoryBytes, 10),
			"--memory-swap", strconv.FormatInt(s.MemoryBytes, 10), "--cpus", strconv.FormatFloat(float64(s.CPUMilli)/1000, 'f', 3, 64),
			"--tmpfs", "/tmp:rw,noexec,nosuid,nodev,size=16777216,uid=1000,gid=1000,mode=700"}
		if s.Role == roleAnchor {
			args = append(args, "--user", "0:0", "--cap-add", "NET_ADMIN")
		} else {
			args = append(args, "--user", nativeUser)
		}
		if s.Role == roleAgent {
			args = append(args, "--tmpfs", "/home/agent:rw,noexec,nosuid,nodev,size=16777216,uid=1000,gid=1000,mode=700")
		}
		if s.Volume != nil {
			if _, err = e.volume(ctx, *s.Volume); err != nil {
				return Instance{}, err
			}
			args = append(args, "--mount", "type=volume,source="+s.Volume.Name+",target=/workspace,volume-nocopy")
		}
		args = append(args, "--entrypoint", cmd[0], s.ImageID)
		args = append(args, cmd[1:]...)
		data, err := e.command(ctx, args...)
		if err != nil {
			return Instance{}, err
		}
		id := strings.TrimSpace(string(data))
		if !objectID.MatchString(id) {
			return Instance{}, errors.New("invalid managed Docker incarnation")
		}
		instance = Instance{ID: id, Execution: s.Execution}
	}
	if err = e.VerifyManaged(ctx, instance, s); err != nil {
		return Instance{}, err
	}
	return instance, nil
}
func (e *Engine) lookup(ctx context.Context, name, execution string) (Instance, bool, error) {
	data, err := e.command(ctx, "container", "ls", "--all", "--no-trunc", "--filter", "name=^/"+name+"$", "--format", "{{.ID}}")
	if err != nil {
		return Instance{}, false, err
	}
	ids := strings.Fields(string(data))
	if len(ids) == 0 {
		return Instance{}, false, nil
	}
	if len(ids) != 1 || !objectID.MatchString(ids[0]) {
		return Instance{}, false, errors.New("ambiguous Docker named resource")
	}
	return Instance{ID: ids[0], Execution: execution}, true, nil
}
func (e *Engine) VerifyManaged(ctx context.Context, i Instance, s ManagedSpec) error {
	if _, err := e.Observe(ctx, i); err != nil {
		return err
	}
	data, err := e.command(ctx, "container", "inspect", i.ID)
	if err != nil {
		return err
	}
	var rows []managedInspection
	if json.Unmarshal(data, &rows) != nil || len(rows) != 1 {
		return errors.New("docker security observation unavailable")
	}
	r := rows[0]
	h := r.HostConfig
	cmd, err := s.command()
	if err != nil {
		return err
	}
	if !matchesManagedLaunch(r, i, s, cmd) || !matchesManagedResources(h, s) {
		return errors.New("docker managed security or resource contract changed")
	}

	if len(h.SecurityOpt) != 1 || (h.SecurityOpt[0] != "no-new-privileges" && h.SecurityOpt[0] != "no-new-privileges:true") {
		return errors.New("docker privilege boundary changed")
	}
	tmpfs := map[string]string{"/tmp": "rw,noexec,nosuid,nodev,size=16777216,uid=1000,gid=1000,mode=700"}
	if s.Role == roleAgent {
		tmpfs["/home/agent"] = tmpfs["/tmp"]
	}
	if !reflect.DeepEqual(h.Tmpfs, tmpfs) {
		return errors.New("docker temporary storage contract changed")
	}
	if s.Volume == nil {
		if len(r.Mounts) != 0 {
			return errors.New("docker unexpected mount")
		}
	} else {
		if len(r.Mounts) != 1 || r.Mounts[0].Type != volumeCommand || r.Mounts[0].Name != s.Volume.Name || r.Mounts[0].Destination != "/workspace" || !r.Mounts[0].RW {
			return errors.New("docker workspace mount changed")
		}
		if _, err = e.volume(ctx, *s.Volume); err != nil {
			return err
		}
	}
	return nil
}
func (e *Engine) NativeOperation(ctx context.Context, i Instance, op string, input []byte) ([]byte, error) {
	switch op {
	case "activate", "status", quiesceCommand, "gate":
		if len(input) != 0 {
			return nil, errors.New("unexpected native operation input")
		}
	case "configure":
		if len(input) > 256<<10 {
			return nil, errors.New("native configuration exceeds limit")
		}
	default:
		return nil, errors.New("unknown fixed native operation")
	}
	return e.exec(ctx, i, []string{nativeCommandPath, op}, input, 64<<10)
}
func (e *Engine) CaptureCheckpoint(ctx context.Context, i Instance, kind harness.NativeKind, thread string, includeIgnored []string, maxBytes int64) ([]byte, error) {
	if err := workspace.ValidateIgnoredPaths(includeIgnored); err != nil {
		return nil, err
	}
	dir, kindErr := harness.NativeDirectory(kind)
	if kindErr != nil || !resourceName.MatchString(thread) || maxBytes <= 0 || maxBytes > 1<<30 {
		return nil, errors.New("invalid checkpoint capture selection")
	}
	args := []string{checkpointCommandPath, "capture-stopped", harnessFlag, string(kind), "--home", "/workspace/session/" + dir, "--workspace", "/workspace/session/workspace", "--thread", thread, maxBytesFlag, strconv.FormatInt(maxBytes, 10)}
	for _, name := range includeIgnored {
		args = append(args, "--include-ignored", name)
	}
	return e.exec(ctx, i, args, nil, int(maxBytes))
}
func (e *Engine) exec(ctx context.Context, i Instance, command []string, input []byte, maxOutput int) ([]byte, error) {
	state, err := e.Observe(ctx, i)
	if err != nil {
		return nil, err
	}
	if !state.Running || state.PID <= 0 {
		return nil, errors.New("docker fixed operation requires its running incarnation")
	}
	args := make([]string, 0, 3+len(command))
	args = append(args, execCommand, "--interactive", i.ID)
	args = append(args, command...)
	return e.commandInput(ctx, input, maxOutput, args...)
}
func (e *Engine) commandInput(ctx context.Context, input []byte, maxOutput int, args ...string) ([]byte, error) {
	if e.execute != nil {
		return e.execute(ctx, input, maxOutput, args)
	}
	cmd := exec.CommandContext(ctx, e.binary, append([]string{"--host", "unix://" + e.socket, "--config", e.config}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + e.config, "LC_ALL=C"}
	cmd.Stdin = bytes.NewReader(input)
	out, errout := &boundedOutput{limit: maxOutput}, &boundedOutput{limit: 4096}
	cmd.Stdout, cmd.Stderr = out, errout
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker fixed operation failed: %w", err)
	}
	return io.ReadAll(out)
}

func (e *Engine) PrepareArtifact(ctx context.Context, i Instance, kind harness.NativeKind, data []byte, maxBytes int64, digest string, initial bool) error {
	if kind.Validate() != nil || maxBytes <= 0 || int64(len(data)) > maxBytes {
		return errors.New("artifact restore exceeds limit")
	}
	command := []string{checkpointCommandPath, prepareCommand, harnessFlag, string(kind), maxBytesFlag, strconv.FormatInt(maxBytes, 10), "--instruction-digest", digest}
	if initial {
		command = []string{checkpointCommandPath, "prepare-workspace", harnessFlag, string(kind), maxBytesFlag, strconv.FormatInt(maxBytes, 10)}
	}
	output, err := e.exec(ctx, i, command, data, 64<<10)
	if err != nil {
		return err
	}
	var receipt struct {
		SHA256 string `json:"sha256"`
		Bytes  int    `json:"bytes"`
	}
	if json.Unmarshal(output, &receipt) != nil || receipt.SHA256 != fmt.Sprintf("%x", sha256.Sum256(data)) || receipt.Bytes != len(data) {
		return errors.New("trusted helper restore receipt did not match input")
	}
	return nil
}
func (e *Engine) FinishHelper(ctx context.Context, i Instance) error {
	_, err := e.exec(ctx, i, []string{checkpointCommandPath, "finish"}, nil, 4096)
	return err
}
func (e *Engine) CheckAnchor(ctx context.Context, i Instance, ip string, port uint16) error {
	_, err := e.exec(ctx, i, []string{networkCommandPath, "check", ip, strconv.Itoa(int(port))}, nil, 4096)
	return err
}
func (e *Engine) AttachNative(ctx context.Context, i Instance, input io.Reader, output io.Writer) error {
	state, err := e.Observe(ctx, i)
	if err != nil {
		return err
	}
	if !state.Running || state.PID <= 0 {
		return errors.New("native attachment requires running bound container")
	}
	args := []string{"--host", "unix://" + e.socket, "--config", e.config, execCommand, "--interactive", i.ID, nativeCommandPath, "attach"}
	cmd := exec.CommandContext(ctx, e.binary, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + e.config, "LC_ALL=C"}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = input, output, &boundedOutput{limit: 4096}
	if err = cmd.Run(); err != nil {
		return errors.New("docker native attachment failed")
	}
	return nil
}

type managedHost struct {
	NetworkMode                         string
	ReadonlyRootfs, Privileged          bool
	CapDrop, CapAdd, SecurityOpt, Binds []string
	Memory, MemorySwap, NanoCpus        int64
	PidsLimit                           *int64
	Tmpfs                               map[string]string
}
type managedInspection struct {
	ID, Image, Name string
	Config          struct {
		User            string
		Cmd, Entrypoint []string
		Labels          map[string]string
	}
	HostConfig managedHost
	Mounts     []struct {
		Type, Name, Destination string
		RW                      bool
	}
}

func matchesManagedLaunch(r managedInspection, i Instance, s ManagedSpec, cmd []string) bool {
	user := nativeUser
	capsMatch := len(r.HostConfig.CapAdd) == 0
	if s.Role == roleAnchor {
		user = "0:0"
		// Docker versions report either the short capability name or its
		// canonical Linux spelling. Neither permits an additional capability.
		capsMatch = len(r.HostConfig.CapAdd) == 1 &&
			(r.HostConfig.CapAdd[0] == "NET_ADMIN" || r.HostConfig.CapAdd[0] == canonicalNetAdmin)
	}
	return r.ID == i.ID && r.Config.Labels[launchLabel] == s.IntentDigest && r.Image == s.ImageID &&
		r.Name == "/"+s.Name && r.Config.User == user && reflect.DeepEqual(r.Config.Cmd, cmd[1:]) &&
		reflect.DeepEqual(r.Config.Entrypoint, cmd[:1]) && r.HostConfig.ReadonlyRootfs && !r.HostConfig.Privileged &&
		r.HostConfig.NetworkMode == s.NetworkMode && reflect.DeepEqual(r.HostConfig.CapDrop, []string{allCapabilities}) &&
		capsMatch && len(r.HostConfig.Binds) == 0
}
func matchesManagedResources(h managedHost, s ManagedSpec) bool {
	return h.Memory == s.MemoryBytes && h.MemorySwap == s.MemoryBytes && h.NanoCpus == s.CPUMilli*1000000 &&
		h.PidsLimit != nil && *h.PidsLimit == s.PIDs
}
