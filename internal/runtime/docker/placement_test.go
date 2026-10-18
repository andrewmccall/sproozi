package docker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"

	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

const fixtureInstructions = "Approved test instructions."
const (
	modelFixtureCapability  = "model"
	threadField             = "threadId"
	stateField              = "state"
	typeField               = "type"
	containerFixtureCommand = "container"
)

const fixtureThread = "01234567-89ab-cdef-0123-456789abcdef"

type fakeContainer struct {
	ID, Image, Name string
	Config          struct {
		User            string
		Cmd, Entrypoint []string
		Labels          map[string]string
	}
	HostConfig struct {
		NetworkMode                         string
		ReadonlyRootfs, Privileged          bool
		CapDrop, CapAdd, SecurityOpt, Binds []string
		Memory, MemorySwap, NanoCpus        int64
		PidsLimit                           *int64
		Tmpfs                               map[string]string
	}
	Mounts []struct {
		Type, Name, Destination string
		RW                      bool
	}
	State struct {
		Running bool
		Status  string
		Pid     int
	}
	nativeState harness.NativeState
	configured  []byte
}
type fakeDocker struct {
	containers                               map[string]*fakeContainer
	volumes                                  map[string]map[string]any
	next                                     int
	capture                                  []byte
	failStop, failCapture, failActivationAck bool
	failRemove                               bool
	rejectNativeActivation                   bool
	quiesceAcknowledgesOnly                  bool
	lostQuiesceAck                           bool
	quiesceStarted                           chan struct{}
	completeTurn                             <-chan struct{}
	calls                                    []string
}

func newFakeDocker() *fakeDocker {
	return &fakeDocker{containers: map[string]*fakeContainer{}, volumes: map[string]map[string]any{}}
}

// running reports a present, running container. A removed container is not
// running; tests that need its absence assert that separately.
func (f *fakeDocker) running(id string) bool {
	c := f.containers[id]
	return c != nil && c.State.Running
}
func (f *fakeDocker) run(ctx context.Context, input []byte, _ int, args []string) ([]byte, error) {
	f.calls = append(f.calls, strings.Join(args, " "))
	switch args[0] {
	case volumeCommand:
		return f.volumeCommand(args)
	case createCommand:
		return f.createCommand(args)
	case containerFixtureCommand:
		return f.containerCommand(args)
	case execCommand:
		return f.execCommand(ctx, args, input)
	}
	return nil, errors.New("unexpected Docker command")
}
func (f *fakeDocker) volumeCommand(args []string) ([]byte, error) {
	if args[1] == createCommand {
		name := args[3]
		if f.volumes[name] == nil {
			f.volumes[name] = map[string]any{"Name": name, "Driver": localVolumeDriver, "CreatedAt": "2026-10-11T12:00:00Z", "Labels": map[string]string{}, "Options": map[string]string{}}
			for i := 4; i < len(args)-1; i++ {
				if args[i] == labelFlag {
					k, v, _ := strings.Cut(args[i+1], "=")
					f.volumes[name]["Labels"].(map[string]string)[k] = v
				}
				if args[i] == "--opt" {
					k, v, _ := strings.Cut(args[i+1], "=")
					f.volumes[name]["Options"].(map[string]string)[k] = v
				}
			}
		}
		return []byte(name), nil
	}
	switch args[1] {
	case "ls":
		var names []string
		for name := range f.volumes {
			if strings.Contains(name, strings.TrimPrefix(args[4], "name=")) {
				names = append(names, name)
			}
		}
		return []byte(strings.Join(names, "\n")), nil
	case "rm":
		if f.failRemove {
			return nil, errors.New("engine volume removal failed")
		}
		if f.volumes[args[2]] == nil {
			return nil, errors.New("volume unavailable")
		}
		for _, c := range f.containers {
			for _, m := range c.Mounts {
				if m.Name == args[2] {
					return nil, errors.New("volume in use")
				}
			}
		}
		delete(f.volumes, args[2])
		return nil, nil
	}
	v := f.volumes[args[2]]
	if v == nil {
		return nil, errors.New("volume unavailable")
	}
	return json.Marshal([]any{v})
}
func (f *fakeDocker) createCommand(args []string) ([]byte, error) {
	f.next++
	c := &fakeContainer{ID: fmt.Sprintf("%064x", f.next)}
	c.State.Status = "created"
	c.Config.Labels = map[string]string{}
	c.HostConfig.Tmpfs = map[string]string{}
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "sha256:") {
			c.Image = arg
			c.Config.Cmd = args[i+1:]
			break
		}
		if i+1 >= len(args) {
			return nil, errors.New("incomplete create flags")
		}
		v := args[i+1]
		i++
		number, _ := strconv.ParseInt(v, 10, 64)
		switch arg {
		case "--name":
			c.Name = "/" + v
		case labelFlag:
			k, value, _ := strings.Cut(v, "=")
			c.Config.Labels[k] = value
		case "--network":
			c.HostConfig.NetworkMode = v
		case "--read-only":
			c.HostConfig.ReadonlyRootfs = true
			i--
		case "--cap-drop":
			c.HostConfig.CapDrop = append(c.HostConfig.CapDrop, v)
		case "--cap-add":
			c.HostConfig.CapAdd = append(c.HostConfig.CapAdd, v)
		case "--security-opt":
			c.HostConfig.SecurityOpt = append(c.HostConfig.SecurityOpt, v)
		case "--user":
			c.Config.User = v
		case "--entrypoint":
			c.Config.Entrypoint = []string{v}
		case "--memory":
			c.HostConfig.Memory = number
		case "--memory-swap":
			c.HostConfig.MemorySwap = number
		case "--pids-limit":
			c.HostConfig.PidsLimit = &number
		case "--cpus":
			cpu, _ := strconv.ParseFloat(v, 64)
			c.HostConfig.NanoCpus = int64(cpu * 1e9)
		case "--tmpfs":
			k, value, _ := strings.Cut(v, ":")
			c.HostConfig.Tmpfs[k] = value
		case "--mount":
			var name string
			for field := range strings.SplitSeq(v, ",") {
				if source, ok := strings.CutPrefix(field, "source="); ok {
					name = source
				}
			}
			c.Mounts = append(c.Mounts, struct {
				Type, Name, Destination string
				RW                      bool
			}{volumeCommand, name, "/workspace", true})
		}
	}
	f.containers[c.ID] = c
	return []byte(c.ID), nil
}
func (f *fakeDocker) containerCommand(args []string) ([]byte, error) {
	if args[1] == "ls" {
		filter := strings.TrimSuffix(strings.TrimPrefix(args[5], "name=^/"), "$")
		for _, c := range f.containers {
			if c.Name == "/"+filter {
				return []byte(c.ID), nil
			}
		}
		return nil, nil
	}
	c := f.containers[args[len(args)-1]]
	if c == nil {
		return nil, errors.New("container unavailable")
	}
	switch args[1] {
	case "inspect":
		return json.Marshal([]any{c})
	case "start":
		c.State.Running = true
		c.State.Status = "running"
		c.State.Pid = f.next + 10
		return []byte(c.ID), nil
	case "stop":
		if f.failStop {
			return nil, errors.New("engine stop failed")
		}
		c.State.Running = false
		c.State.Status = exitedStatus
		c.State.Pid = 0
		return []byte(c.ID), nil
	case "rm":
		if f.failRemove {
			return nil, errors.New("engine container removal failed")
		}
		if c.State.Running {
			return nil, errors.New("container is running")
		}
		delete(f.containers, c.ID)
		return nil, nil
	}
	return nil, errors.New("unexpected fixture operation")
}
func (f *fakeDocker) execCommand(ctx context.Context, args []string, input []byte) ([]byte, error) {
	c := f.containers[args[2]]
	if c == nil || !c.State.Running {
		return nil, errors.New("exec unavailable")
	}
	op := args[4]
	if args[3] == networkCommandPath {
		return nil, nil
	}
	if args[3] == checkpointCommandPath {
		switch op {
		case prepareCommand, "prepare-workspace":
			return json.Marshal(map[string]any{"sha256": fmt.Sprintf("%x", sha256.Sum256(input)), "bytes": len(input)})
		case "capture-stopped":
			if f.failCapture {
				return nil, errors.New("capture failed")
			}
			return f.capture, nil
		case "finish":
			c.State.Running = false
			c.State.Status = exitedStatus
			c.State.Pid = 0
			return nil, nil
		}
	}
	switch op {
	case "configure":
		if c.configured != nil && !strings.EqualFold(string(c.configured), string(input)) {
			return nil, errors.New("configuration changed")
		}
		c.configured = append([]byte{}, input...)
		c.nativeState = harness.NativePrepared
	case "activate":
		if f.rejectNativeActivation {
			return json.Marshal(map[string]any{"Error": "native version gate rejected activation", stateField: harness.NativePrepared})
		}
		c.nativeState = harness.NativeActive
		if f.failActivationAck {
			f.failActivationAck = false
			return nil, errors.New("activation acknowledgement lost")
		}
	case "gate":
		c.nativeState = harness.NativeStopping
	case "status":
		return json.Marshal(map[string]any{"op": op, stateField: c.nativeState, threadField: fixtureThread})
	case string(harness.NativeQuiesce):
		if f.quiesceStarted != nil {
			close(f.quiesceStarted)
			select {
			case <-f.completeTurn:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if f.quiesceAcknowledgesOnly {
			return json.Marshal(map[string]any{"op": op, stateField: harness.NativeStopped, threadField: fixtureThread})
		}
		c.State.Running = false
		c.State.Status = exitedStatus
		c.State.Pid = 0
		if f.lostQuiesceAck {
			return nil, errors.New("lost quiesce acknowledgement")
		}
		return json.Marshal(map[string]any{"op": op, stateField: harness.NativeStopped, threadField: fixtureThread})
	}
	var launch harness.NativeLaunch
	if err := json.Unmarshal(c.configured, &launch); err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"op": op, stateField: c.nativeState, threadField: launch.ThreadID})
}

type fakeAuthority struct {
	engine                 *fakeDocker
	active, revoked, bound bool
	sealFail               bool
	init, activate, seals  int
	activateErr            error
}

func (a *fakeAuthority) InitializeAllocation(context.Context, session.Launch, session.Incarnation) (NativeAccess, error) {
	a.init++
	return NativeAccess{Env: []string{"PATH=/usr/bin:/bin"}}, nil
}
func (a *fakeAuthority) Activate(context.Context, session.Launch, session.Incarnation) error {
	if a.activateErr != nil {
		return a.activateErr
	}
	if a.revoked {
		return errors.New("authority already revoked")
	}
	a.active = true
	a.activate++
	return nil
}
func (a *fakeAuthority) Revoke(context.Context, session.Launch, session.Incarnation) error {
	a.revoked = true
	a.active = false
	return nil
}
func (a *fakeAuthority) BindNative(_ context.Context, l session.Launch, _ session.Incarnation, id string) error {
	if l.ThreadID != "" || id != fixtureThread {
		return errors.New("invalid first binding")
	}
	a.bound = true
	return nil
}
func (a *fakeAuthority) Seal(_ context.Context, _ session.Launch, i session.Incarnation) (map[string]budget.TrustedUsage, error) {
	a.seals++
	if a.engine.containers[i.ResourceUID].State.Running {
		return nil, errors.New("sealed before actual stop")
	}
	if a.sealFail {
		return nil, errors.New("ledger unavailable")
	}
	return map[string]budget.TrustedUsage{modelFixtureCapability: {Units: 17, CostMicros: 23}}, nil
}
func fixtureConfig() PlacementConfig {
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(fixtureInstructions)))
	limits := ResourceLimits{MemoryBytes: 128 << 20, CPUMilli: 500, PIDs: 64}
	return PlacementConfig{Harness: harness.NativeCodex, Name: localVolumeDriver, NativeImageID: "sha256:" + strings.Repeat("a", 64), HelperImageID: "sha256:" + strings.Repeat("b", 64), AnchorImageID: "sha256:" + strings.Repeat("c", 64), NetworkName: fixtureBridge, GatewayIPv4: "192.168.65.254", GatewayPort: 8844, Instructions: fixtureInstructions, InstructionDigest: hash, AgentLimits: limits, HelperLimits: limits, AnchorLimits: limits, VolumeBytes: 64 << 20, MaxCheckpointBytes: 1 << 20, RetainedBytes: 8 << 20}
}

// builtArtifact is the stopped checkpoint every test starts from. Its Git
// capture is slow, so it is built once per test binary.
var builtArtifact struct {
	once  sync.Once
	built bool
	data  []byte
}

func fixtureArtifact(t *testing.T) []byte {
	t.Helper()
	builtArtifact.once.Do(func() {
		builtArtifact.data = buildFixtureArtifact(t)
		builtArtifact.built = true
	})
	if !builtArtifact.built {
		t.Fatal("fixture checkpoint failed to build in another test")
	}
	return bytes.Clone(builtArtifact.data)
}

func buildFixtureArtifact(t *testing.T) []byte {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repository")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + root, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@invalid"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git fixture: %v %s", err, out)
		}
	}
	git("init")
	if err := os.WriteFile(filepath.Join(repo, "fact.txt"), []byte("fixture fact\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "fact.txt")
	git("commit", "-m", "fixture")
	home := filepath.Join(root, "codex")
	dir := filepath.Join(home, "sessions", "2026", "10", "11")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	meta, _ := json.Marshal(map[string]any{typeField: "session_meta", "payload": map[string]any{"id": fixtureThread, "cli_version": "0.161.0", "cwd": repo}})
	dev, _ := json.Marshal(map[string]any{typeField: "response_item", "payload": map[string]any{typeField: "message", "role": "developer", "content": []map[string]string{{typeField: "input_text", "text": fixtureInstructions}}}})
	journal := string(meta) + "\n" + string(dev) + `\n`
	journal = strings.TrimSuffix(journal, `\n`) + "\n" + `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-one"}}` + "\n" + `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-one"}}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-10-11T12-00-00-"+fixtureThread+".jsonl"), []byte(journal), 0600); err != nil {
		t.Fatal(err)
	}
	data, _, err := checkpoint.CaptureStopped(context.Background(), harness.NativeCodex, home, repo, fixtureThread, nil, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
func fixtureLaunch(t *testing.T, data []byte) session.Launch {
	t.Helper()
	meta, err := checkpoint.Inspect(data, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return session.Launch{Harness: harness.NativeCodex, ExecutionID: uuid.NewString(), OperationID: uuid.NewString(), AllocationID: uuid.NewString(), Epoch: 1, Placement: localVolumeDriver, ThreadID: fixtureThread, InstructionDigest: fixtureConfig().InstructionDigest, Deadline: time.Now().UTC().Add(time.Minute), Allowances: map[string]session.Allowance{modelFixtureCapability: {Units: session.Limit{Bounded: true, Value: 100}, CostMicros: session.Limit{Bounded: true, Value: 1000}}}, CheckpointDigest: meta.SHA256}
}
func openFixture(t *testing.T, dir string, f *fakeDocker, a *fakeAuthority) *Placement {
	t.Helper()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	p, err := OpenPlacement(dir, &Engine{execute: f.run}, fixtureConfig(), a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

func TestIgnoredSelectionBindsDockerVisitAndFixedCapture(t *testing.T) {
	t.Parallel()
	data := fixtureArtifact(t)
	l := fixtureLaunch(t, data)
	l.IncludeIgnored = []string{"needed.bin", "cache/chosen.bin"}
	f := newFakeDocker()
	f.capture = data
	p := openFixture(t, t.TempDir(), f, &fakeAuthority{engine: f})
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	changed := l
	changed.IncludeIgnored = []string{"other.bin"}
	if _, err = p.Prepare(t.Context(), changed, data); err == nil {
		t.Fatal("retained Docker operation accepted changed ignored selection")
	}
	if err = p.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil {
		t.Fatal(err)
	}
	capture := ""
	for _, call := range f.calls {
		if strings.Contains(call, "/usr/local/bin/checkpoint capture-stopped ") {
			capture = call
		}
	}
	if !strings.HasSuffix(capture, "--include-ignored needed.bin --include-ignored cache/chosen.bin") {
		t.Fatalf("fixed helper did not receive selected ignored paths: %s", capture)
	}
	if strings.Join(stopped.Receipt.Launch.IncludeIgnored, ",") != "needed.bin,cache/chosen.bin" {
		t.Fatal("trusted stop receipt dropped selection")
	}
}

func TestPlacementDurablePrepareActivateStop(t *testing.T) {
	data := fixtureArtifact(t)
	l := fixtureLaunch(t, data)
	f := newFakeDocker()
	f.capture = data
	a := &fakeAuthority{engine: f}
	dir := t.TempDir()
	p := openFixture(t, dir, f, a)
	inc, err := p.Prepare(context.Background(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if a.active {
		t.Fatal("preparation activated gateway")
	}
	again, err := p.Prepare(context.Background(), l, data)
	if err != nil || again != inc || f.next != 3 {
		t.Fatalf("prepare replay: %+v %v", again, err)
	}
	changed := l
	changed.Epoch++
	if _, err = p.Prepare(context.Background(), changed, data); err == nil {
		t.Fatal("changed intent adopted resources")
	}
	if err = p.Close(); err != nil {
		t.Fatal(err)
	}
	p = openFixture(t, dir, f, a)
	f.failActivationAck = true
	if err = p.Activate(context.Background(), l, inc); err == nil {
		t.Fatal("lost activation response hidden")
	}
	if err = p.Activate(context.Background(), l, inc); err != nil {
		t.Fatal(err)
	}
	if f.next != 3 || !a.active {
		t.Fatal("activation replay created replacement")
	}
	stopped, err := p.Stop(context.Background(), l, inc)
	if err != nil {
		t.Fatal(err)
	}
	if !stopped.ActualStopped || stopped.Receipt == nil || stopped.Receipt.Usage[modelFixtureCapability].Units != 17 || stopped.CheckpointDigest != l.CheckpointDigest {
		t.Fatalf("stopped receipt: %+v", stopped)
	}
	if !a.revoked || f.running(inc.ResourceUID) {
		t.Fatal("source authority or native writers remain")
	}
	if len(f.containers) != 0 || len(f.volumes) != 0 {
		t.Fatalf("stopped visit left resources: %d containers, %d volumes", len(f.containers), len(f.volumes))
	}
	before := a.seals
	_ = p.Close()
	p = openFixture(t, dir, f, a)
	replay, err := p.Stop(context.Background(), l, inc)
	if err != nil || replay.CheckpointDigest != stopped.CheckpointDigest || a.seals != before {
		t.Fatalf("immutable stop replay: %+v %v", replay, err)
	}
	if err = p.Activate(context.Background(), l, inc); err == nil {
		t.Fatal("stopped allocation reopened")
	}
}
func TestUnknownStopAndUsageNeverBecomeZero(t *testing.T) {
	t.Parallel()
	data := fixtureArtifact(t)
	l := fixtureLaunch(t, data)
	f := newFakeDocker()
	f.capture = data
	a := &fakeAuthority{engine: f}
	p := openFixture(t, t.TempDir(), f, a)
	inc, err := p.Prepare(context.Background(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(context.Background(), l, inc); err != nil {
		t.Fatal(err)
	}
	a.sealFail = true
	stopped, err := p.Stop(context.Background(), l, inc)
	if !errors.Is(err, session.ErrUsageUnsealed) || !stopped.ActualStopped || stopped.Receipt != nil {
		t.Fatalf("unknown accounting: %+v %v", stopped, err)
	}
	a.sealFail = false
	stopped, err = p.Stop(context.Background(), l, inc)
	if err != nil || stopped.Receipt == nil {
		t.Fatalf("accounting recovery: %+v %v", stopped, err)
	}
}
func TestIncarnationQuotaAndOwnerChecks(t *testing.T) {
	t.Parallel()
	data := fixtureArtifact(t)
	l := fixtureLaunch(t, data)
	f := newFakeDocker()
	a := &fakeAuthority{engine: f}
	dir := t.TempDir()
	p := openFixture(t, dir, f, a)
	inc, err := p.Prepare(context.Background(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = OpenPlacement(dir, &Engine{execute: f.run}, fixtureConfig(), a); err == nil {
		t.Fatal("second placement owner accepted")
	}
	wrong := inc
	wrong.ResourceUID = strings.Repeat("f", 64)
	if err = p.Activate(context.Background(), l, wrong); err == nil {
		t.Fatal("wrong incarnation activated")
	}
	for _, v := range f.volumes {
		v["CreatedAt"] = "replacement"
	}
	if err = p.Activate(context.Background(), l, inc); err == nil {
		t.Fatal("replaced quota volume adopted")
	}
}

func TestInitialNativeGateRejectionCanAbortWithoutActivationOrCheckpoint(t *testing.T) {
	t.Parallel()
	data := fixtureArtifact(t)
	l := fixtureLaunch(t, data)
	l.ThreadID, l.CheckpointDigest = "", ""
	f := newFakeDocker()
	f.rejectNativeActivation, f.failCapture = true, true
	a := &fakeAuthority{engine: f}
	p := openFixture(t, t.TempDir(), f, a)
	inc, err := p.Prepare(t.Context(), l, []byte("workspace-continuity"))
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(t.Context(), l, inc); err == nil {
		t.Fatal("native version gate failure hidden")
	}
	o, err := p.Abort(t.Context(), l, inc)
	if err != nil || o.State != session.PlacementStopped || o.Stopped == nil || !o.Stopped.ActualStopped {
		t.Fatalf("rejected activation abort: %+v %v", o, err)
	}
	if !a.revoked || a.active || a.activate != 1 || a.bound || o.Launch.ThreadID != "" ||
		o.Stopped.CheckpointDigest != "" || len(o.Stopped.Checkpoint) != 0 || o.Stopped.Receipt == nil {
		t.Fatal("abort reopened authority, fabricated native identity or required a checkpoint")
	}
	for _, container := range f.containers {
		if container.State.Running || container.State.Pid != 0 {
			t.Fatal("abort left an execution writer running")
		}
	}
	seals := a.seals
	if _, err = p.Abort(t.Context(), l, inc); err != nil || a.seals != seals {
		t.Fatalf("abort replay replaced sealed receipt: %v", err)
	}
	if err = p.Activate(t.Context(), l, inc); err == nil || a.activate != 1 {
		t.Fatal("aborted initial allocation reopened")
	}
}

func TestFirstLocalSessionBindsGeneratedNativeUUID(t *testing.T) {
	t.Parallel()
	data := fixtureArtifact(t)
	l := fixtureLaunch(t, data)
	l.ThreadID = ""
	l.CheckpointDigest = ""
	f := newFakeDocker()
	f.capture = data
	a := &fakeAuthority{engine: f}
	p := openFixture(t, t.TempDir(), f, a)
	inc, err := p.Prepare(context.Background(), l, []byte("workspace-continuity"))
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(context.Background(), l, inc); err != nil {
		t.Fatal(err)
	}
	stopped, err := p.Stop(context.Background(), l, inc)
	if err != nil {
		t.Fatal(err)
	}
	if !a.bound || stopped.ThreadID != fixtureThread || stopped.Receipt == nil {
		t.Fatalf("first local binding: %+v", stopped)
	}
	if !session.SameLaunch(stopped.Launch, l) || !session.SameLaunch(stopped.Receipt.Launch, l) {
		t.Fatalf("binding rewrote the immutable launch: %+v", stopped.Launch)
	}
	replay, err := p.Stop(context.Background(), l, inc)
	if err != nil || replay.CheckpointDigest != stopped.CheckpointDigest || replay.ThreadID != fixtureThread {
		t.Fatalf("launch replay: %+v %v", replay, err)
	}
	rewritten := l
	rewritten.ThreadID = fixtureThread
	if _, err = p.Stop(context.Background(), rewritten, inc); err == nil {
		t.Fatal("a launch rewritten with the bound thread replayed the stop")
	}
}
func TestCaptureFailureKeepsActualStopAndBlocksActivation(t *testing.T) {
	t.Parallel()
	data := fixtureArtifact(t)
	l := fixtureLaunch(t, data)
	f := newFakeDocker()
	f.capture = data
	f.failCapture = true
	a := &fakeAuthority{engine: f}
	p := openFixture(t, t.TempDir(), f, a)
	inc, err := p.Prepare(context.Background(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(context.Background(), l, inc); err != nil {
		t.Fatal(err)
	}
	if _, err = p.Stop(context.Background(), l, inc); err == nil {
		t.Fatal("failed capture became checkpoint")
	}
	o, err := p.Inspect(context.Background(), l)
	if err != nil || o.State != session.PlacementStopped || !o.Stopped.ActualStopped || len(o.Stopped.Checkpoint) != 0 {
		t.Fatalf("retained actual fence: %+v %v", o, err)
	}
	if err = p.Activate(context.Background(), l, inc); err == nil {
		t.Fatal("capture failure reopened source")
	}
	f.failCapture = false
	if stopped, err := p.Stop(context.Background(), l, inc); err != nil || len(stopped.Checkpoint) == 0 {
		t.Fatalf("capture recovery: %+v %v", stopped, err)
	}
}

func TestNativeAcknowledgementIsNotAWriterFence(t *testing.T) {
	t.Parallel()
	data := fixtureArtifact(t)
	l := fixtureLaunch(t, data)
	f := newFakeDocker()
	f.capture = data
	a := &fakeAuthority{engine: f}
	p := openFixture(t, t.TempDir(), f, a)
	inc, err := p.Prepare(context.Background(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(context.Background(), l, inc); err != nil {
		t.Fatal(err)
	}
	f.quiesceAcknowledgesOnly = true
	f.failStop = true
	if _, err = p.Stop(context.Background(), l, inc); err == nil {
		t.Fatal("native stop reply accepted without engine fence")
	}
	if a.seals != 0 || !f.containers[inc.ResourceUID].State.Running {
		t.Fatal("usage settled before independent actual stop")
	}
	f.failStop = false
	stopped, err := p.Stop(context.Background(), l, inc)
	if err != nil || !stopped.ActualStopped || stopped.Receipt == nil {
		t.Fatalf("actual fence recovery: %+v %v", stopped, err)
	}
}

func TestPreparedNativeLaunchCarriesTaskAndIndependentRolloutLimit(t *testing.T) {
	t.Parallel()
	data := fixtureArtifact(t)
	engine := newFakeDocker()
	p := openFixture(t, t.TempDir(), engine, &fakeAuthority{engine: engine})
	launch := fixtureLaunch(t, data)
	launch.Task = "Inspect the workspace and report the result."
	if _, err := p.Prepare(context.Background(), launch, data); err != nil {
		t.Fatal(err)
	}
	var configured harness.NativeLaunch
	for _, container := range engine.containers {
		if len(container.configured) > 0 {
			if err := json.Unmarshal(container.configured, &configured); err != nil {
				t.Fatal(err)
			}
		}
	}
	if configured.Task != launch.Task || configured.ThreadID != fixtureThread || configured.RolloutMaxBytes != 64<<20 || configured.RolloutMaxBytes == p.config.MaxCheckpointBytes {
		t.Fatalf("native task/thread/observer bound lost: %+v", configured)
	}
}
