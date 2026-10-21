package home

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/testutil"

	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/harness"
	runtimedocker "github.com/andrewmccall/sproozi/internal/runtime/docker"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/workspace"
	"github.com/google/uuid"
)

const testNativeUUID = "01a126e8-288d-7350-9b52-627e16048343"
const testCapability = "model.inference"
const testInstructions = "fixture approved developer instruction"
const testLimit = int64(8 << 20)
const testLocal = "local"
const testRemote = "remote"
const testClaudeLocal = "local-claude"
const testIgnoredFile = "needed.bin"

type placementFixture struct {
	rows                                           map[string]session.Observation
	artifact                                       []byte
	digest                                         string
	initials, prepares, activations, stops, aborts int
	lostStop, lostActivate, unknownUsage           bool
	// turnless stops a visit from a workspace with no conversation.
	turnless                    bool
	rejectActivate, failInspect bool
	partialCreate               bool
	inspections                 int
	charge                      int64
	// Non-nil hold channels are closed when the call is reached; the call then
	// waits for its context, as a native turn that has not completed would.
	holdPrepare, holdStop chan struct{}
	// abortEntered, when non-nil, is closed by the first Abort.
	abortEntered chan struct{}
}

func (p *placementFixture) prepare(l session.Launch) session.Incarnation {
	if r, ok := p.rows[l.AllocationID]; ok && r.State != session.PlacementPreparing {
		return r.Incarnation
	}
	i := session.Incarnation{ExecutionID: l.AllocationID, ResourceUID: uuid.NewString()}
	p.rows[l.AllocationID] = session.Observation{Launch: l, Incarnation: i, State: session.PlacementPrepared}
	return i
}

// Prepare restores a captured workspace for a first visit, which must not name
// a native thread, or the retained checkpoint for any other visit.
func (p *placementFixture) Prepare(ctx context.Context, l session.Launch, data []byte) (session.Incarnation, error) {
	if l.CheckpointDigest == "" {
		if l.ThreadID != "" || len(data) == 0 {
			return session.Incarnation{}, errors.New("initial session fabricated native state")
		}
		p.initials++
		if p.partialCreate {
			p.rows[l.AllocationID] = session.Observation{Launch: l, State: session.PlacementPreparing}
			return session.Incarnation{}, errors.New("partial initial preparation")
		}
	} else {
		if l.ThreadID != testNativeUUID || l.CheckpointDigest != p.digest || string(data) != string(p.artifact) {
			return session.Incarnation{}, errors.New("native continuity changed")
		}
		p.prepares++
	}
	i := p.prepare(l)
	if p.holdPrepare != nil {
		close(p.holdPrepare)
		<-ctx.Done()
	}
	return i, nil
}
func (p *placementFixture) Activate(_ context.Context, l session.Launch, i session.Incarnation) error {
	r := p.rows[l.AllocationID]
	if r.Incarnation != i {
		return errors.New("wrong incarnation")
	}
	if r.State == session.PlacementActive {
		return nil
	}
	if p.rejectActivate {
		p.activations++
		return errors.New("native version gate rejected activation")
	}
	r.State = session.PlacementActive
	p.rows[l.AllocationID] = r
	p.activations++
	if p.lostActivate {
		p.lostActivate = false
		return errors.New("lost activation reply")
	}
	return nil
}
func (p *placementFixture) Inspect(_ context.Context, l session.Launch) (session.Observation, error) {
	p.inspections++
	if p.failInspect {
		return session.Observation{}, errors.New("activation recovery still fails")
	}
	if r, ok := p.rows[l.AllocationID]; ok {
		return r, nil
	}
	return session.Observation{Launch: l, State: session.PlacementAbsent}, nil
}

// Stop reports the genuine native thread separately; the launch is unchanged.
func (p *placementFixture) Stop(ctx context.Context, l session.Launch, i session.Incarnation) (session.Stopped, error) {
	if hold := p.holdStop; hold != nil {
		p.holdStop = nil
		close(hold)
		<-ctx.Done()
		return session.Stopped{}, ctx.Err()
	}
	r := p.rows[l.AllocationID]
	if r.Incarnation != i {
		return session.Stopped{}, errors.New("wrong stop incarnation")
	}
	if r.Stopped != nil {
		return *r.Stopped, nil
	}
	p.stops++
	stopped := session.Stopped{Launch: l, Incarnation: i, ActualStopped: true, ThreadID: testNativeUUID, Checkpoint: p.artifact, CheckpointDigest: p.digest, Receipt: p.receipt(l, i)}
	if p.unknownUsage {
		stopped.Receipt = nil
	}
	if p.turnless && l.ThreadID == "" {
		stopped.ThreadID, stopped.Checkpoint, stopped.CheckpointDigest = "", nil, ""
	}
	r.State = session.PlacementStopped
	r.Stopped = &stopped
	p.rows[l.AllocationID] = r
	if p.lostStop {
		p.lostStop = false
		return session.Stopped{}, errors.New("lost stop reply")
	}
	return stopped, nil
}
func (p *placementFixture) receipt(l session.Launch, i session.Incarnation) *session.Receipt {
	return &session.Receipt{Launch: l, Incarnation: i, Usage: map[string]budget.TrustedUsage{testCapability: {Units: p.charge}}}
}

// Abort fences the writer and seals its usage, as the Docker placement does
// even when an earlier graceful stop could not seal it.
func (p *placementFixture) Abort(_ context.Context, l session.Launch, i session.Incarnation) (session.Observation, error) {
	p.aborts++
	if p.abortEntered != nil {
		close(p.abortEntered)
		p.abortEntered = nil
	}
	r := p.rows[l.AllocationID]
	if r.Incarnation != i {
		return r, errors.New("wrong abort incarnation")
	}
	r.State = session.PlacementStopped
	r.Stopped = &session.Stopped{Launch: l, Incarnation: i, ActualStopped: true, Receipt: p.receipt(l, i)}
	p.rows[l.AllocationID] = r
	return r, nil
}
func (p *placementFixture) AbortPreparation(ctx context.Context, l session.Launch) error {
	r, found := p.rows[l.AllocationID]
	if !found {
		return nil
	}
	_, err := p.Abort(ctx, l, r.Incarnation)
	return err
}
func (p *placementFixture) Attach(context.Context, session.Launch, session.Incarnation, io.Reader, io.Writer) error {
	return nil
}

// template is the workspace and checkpoint every testService starts from.
// Git makes both slow, so they are built once per test binary; each test
// gets a private copy of the workspace.
var template struct {
	once   sync.Once
	built  bool
	root   string
	repo   string
	data   []byte
	digest string
}

func TestMain(m *testing.M) {
	code := m.Run()
	if template.root != "" {
		_ = os.RemoveAll(template.root)
	}
	os.Exit(code)
}

// testWorkspace returns a private copy of the template workspace and its
// checkpoint. The copy keeps each file's mode, so it captures identically.
func testWorkspace(t *testing.T) (string, []byte, string) {
	t.Helper()
	template.once.Do(func() {
		root, err := os.MkdirTemp("", "sproozi-home-fixture-")
		if err != nil {
			t.Fatal(err)
		}
		template.root = root
		template.repo = filepath.Join(root, "repository")
		createWorkspace(t, template.repo)
		template.data, template.digest = testArtifact(t, template.repo)
		template.built = true
	})
	if !template.built {
		t.Fatal("fixture workspace failed to build in another test")
	}
	repo := filepath.Join(testutil.PrivateDir(t), "repository")
	if err := os.CopyFS(repo, os.DirFS(template.repo)); err != nil {
		t.Fatal(err)
	}
	err := filepath.WalkDir(template.repo, func(path string, _ fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(template.repo, path)
		if err != nil {
			return err
		}
		return os.Chmod(filepath.Join(repo, rel), info.Mode().Perm())
	})
	if err != nil {
		t.Fatal(err)
	}
	return repo, bytes.Clone(template.data), template.digest
}

func createWorkspace(t *testing.T, repo string) {
	t.Helper()
	home := testutil.PrivateDir(t)
	if err := testutil.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "--quiet", "--template=", "-b", "main"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@invalid", "commit", "--quiet", "--allow-empty", "-m", "Fixture"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("Git fixture: %v %s", err, output)
		}
	}
	if err := testutil.WriteFile(filepath.Join(repo, "unfinished.txt"), []byte("unfinished work\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

// Synthetic native files are unit fixtures only; the production verifier owns
// the empirical stock-client proof and cannot use these as mobility evidence.
func testArtifact(t *testing.T, repo string, includeIgnored ...string) ([]byte, string) {
	t.Helper()
	home := testutil.PrivateDir(t)
	state := harness.CodexState{ThreadID: testNativeUUID, Path: "sessions/2026/10/10/rollout-2026-10-10T18-41-51-" + testNativeUUID + ".jsonl", Rollout: []byte(`{"type":"session_meta","payload":{"id":"` + testNativeUUID + `","cli_version":"0.161.0","cwd":"/workspace"}}
{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"` + testInstructions + `"}]}}
{"type":"event_msg","payload":{"type":"task_started","turn_id":"fixture-turn"}}
{"type":"event_msg","payload":{"type":"task_complete","turn_id":"fixture-turn"}}
`)}
	if err := harness.RestoreCodexState(home, state, testLimit); err != nil {
		t.Fatal(err)
	}
	data, meta, err := checkpoint.CaptureStopped(t.Context(), harness.NativeCodex, home, repo, testNativeUUID, includeIgnored, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	return data, meta.SHA256
}

func testService(t *testing.T) (*Service, *placementFixture, *placementFixture, string) {
	t.Helper()
	repo, data, digest := testWorkspace(t)
	local := &placementFixture{rows: map[string]session.Observation{}, artifact: data, digest: digest, charge: 2}
	remote := &placementFixture{rows: map[string]session.Observation{}, artifact: data, digest: digest, charge: 3}
	dir := filepath.Join(testutil.PrivateDir(t), "home")
	config := Config{Directory: dir, Local: runtimedocker.PlacementConfig{Harness: harness.NativeCodex, Name: testLocal, InstructionDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(testInstructions))), MaxCheckpointBytes: testLimit, RetainedBytes: 64 << 20}, Allowances: map[string]session.Allowance{testCapability: {Units: session.Limit{Bounded: true, Value: 10}}}}
	placements := map[string]session.Placement{testLocal: local, testRemote: remote}
	coordinator, err := session.Open(filepath.Join(dir, "journal"), placements, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{config: config, duration: time.Hour, stacks: []*localStack{{config: config.Local}}, placements: placements, coordinator: coordinator}
	t.Cleanup(func() { _ = s.Close() })
	return s, local, remote, repo
}

func TestStartIgnoredOptInRetainsBytesAndImmutableSelectionAcrossVisits(t *testing.T) {
	t.Parallel()
	s, local, remote, repo := testService(t)
	for name, data := range map[string][]byte{
		".gitignore": []byte("needed.bin\n.env\n"), testIgnoredFile: {0, 1, 255, 254},
		".env": []byte("excluded fixture credential"),
	} {
		if err := os.WriteFile(filepath.Join(repo, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	data, digest := testArtifact(t, repo, testIgnoredFile)
	local.artifact, local.digest, remote.artifact, remote.digest = data, digest, data, digest
	id, operation := uuid.NewString(), uuid.NewString()
	if _, err := s.Start(t.Context(), id, operation, repo, "", []string{testIgnoredFile}, nil); err != nil {
		t.Fatal(err)
	}
	execution, err := s.coordinator.Inspect(id)
	if err != nil {
		t.Fatal(err)
	}
	first := execution.Operations[0].Launch
	input, err := os.ReadFile(filepath.Join(s.config.Directory, "journal", "workspaces", execution.Workspace+".workspace"))
	if err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(testutil.PrivateDir(t), "restored")
	if err = workspace.Restore(t.Context(), input, restored, testLimit); err != nil {
		t.Fatal(err)
	}
	selected, err := os.ReadFile(filepath.Join(restored, testIgnoredFile))
	if err != nil || string(selected) != string([]byte{0, 1, 255, 254}) {
		t.Fatalf("initial selection did not preserve binary bytes: %v", err)
	}
	if _, err = os.Stat(filepath.Join(restored, ".env")); !os.IsNotExist(err) {
		t.Fatal("default excluded ignored file was transferred")
	}
	if len(first.IncludeIgnored) != 1 || first.IncludeIgnored[0] != testIgnoredFile {
		t.Fatal("initial launch omitted the approved ignored file")
	}
	if _, err = s.Start(t.Context(), id, operation, repo, "", nil, nil); err == nil {
		t.Fatal("retry removed immutable ignored selection")
	}
	for _, destination := range []string{testRemote, testLocal, testRemote} {
		status, err := s.Move(t.Context(), id, uuid.NewString(), destination)
		if err != nil {
			t.Fatal(err)
		}
		for _, visit := range status.Session.Visits {
			if len(visit.Launch.IncludeIgnored) != 1 || visit.Launch.IncludeIgnored[0] != testIgnoredFile || len(visit.Receipt.Launch.IncludeIgnored) != 1 || visit.Receipt.Launch.IncludeIgnored[0] != testIgnoredFile {
				t.Fatal("completed visit or sealed receipt lost ignored opt-in")
			}
		}
	}
}

func TestInitialSessionImportRoundTripAndLostReplies(t *testing.T) {
	t.Parallel()
	s, local, remote, repo := testService(t)
	id, startOp, moveOp := uuid.NewString(), uuid.NewString(), uuid.NewString()
	local.lostActivate = true
	if _, err := s.Start(t.Context(), id, startOp, repo, "", nil, nil); err == nil {
		t.Fatal("lost activation reply succeeded")
	}
	status, err := s.Recover(t.Context(), id)
	if err != nil || status.Phase != string(session.PhaseActive) || status.ThreadID != "" {
		t.Fatalf("initial recovery: %+v %v", status, err)
	}
	if local.initials != 1 || local.activations != 1 {
		t.Fatal("retry created or activated another initial execution")
	}
	local.lostStop = true
	if _, err = s.Move(t.Context(), id, moveOp, testRemote); err == nil {
		t.Fatal("lost stop reply succeeded")
	}
	if remote.prepares != 0 {
		t.Fatal("destination prepared before source stop acknowledgement")
	}
	if _, err = s.Move(t.Context(), id, uuid.NewString(), testRemote); err == nil {
		t.Fatal("different first operation superseded retained intent")
	}
	status, err = s.Recover(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if status.ThreadID != testNativeUUID || status.Session.Remaining[testCapability].Units.Value != 8 || status.Session.Epoch != 2 {
		t.Fatalf("import accounting: %+v", status)
	}
	if remote.prepares != 1 || remote.activations != 1 || local.stops != 1 {
		t.Fatal("recovery duplicated a placement")
	}
	deadline := status.Deadline
	status, err = s.Move(t.Context(), id, uuid.NewString(), testLocal)
	if err != nil {
		t.Fatal(err)
	}
	if status.Session.Remaining[testCapability].Units.Value != 5 || status.Session.Epoch != 3 || !status.Session.Active.Launch.Deadline.Equal(deadline) {
		t.Fatalf("round trip widened allowance or deadline: %+v", status)
	}
}

// A first move interrupted after its source stopped resumes after a restart
// as the same retained operation; a different move cannot replace it.
func TestRecoverFirstMoveRetainedBeforeRestart(t *testing.T) {
	s, local, remote, repo := testService(t)
	id := uuid.NewString()
	if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	local.lostStop = true
	if _, err := s.Move(t.Context(), id, uuid.NewString(), testRemote); err == nil {
		t.Fatal("lost stop reply succeeded")
	}
	var err error
	if err = s.coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	s.coordinator, err = session.Open(filepath.Join(s.config.Directory, "journal"), s.placements, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Move(t.Context(), id, uuid.NewString(), testLocal); err == nil {
		t.Fatal("retained first move was replaced")
	}
	status, err := s.Recover(t.Context(), id)
	if err != nil || status.Session.Phase != session.PhaseActive || remote.activations != 1 {
		t.Fatalf("retained imported move: %+v %v", status, err)
	}
}

func TestRejectedInitialActivationCancelsWithoutInspectingOrCompletingNativeTurn(t *testing.T) {
	t.Parallel()
	s, local, _, repo := testService(t)
	id := uuid.NewString()
	local.rejectActivate = true
	if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, "", nil, nil); err == nil {
		t.Fatal("native activation rejection hidden")
	}
	retained, err := s.coordinator.Inspect(id)
	if err != nil || retained.Phase != session.PhaseActivating || retained.Operations[0].Incarnation == nil {
		t.Fatalf("failed activation lost exact incarnation: %+v %v", retained, err)
	}
	local.failInspect = true
	inspections := local.inspections
	operation := uuid.NewString()
	status, err := s.Cancel(t.Context(), id, operation)
	if err != nil || status.Phase != string(session.PhaseCancelled) {
		t.Fatalf("rejected initial activation cancellation: %+v %v", status, err)
	}
	if local.inspections != inspections || local.activations != 1 || local.aborts != 1 || local.stops != 0 {
		t.Fatal("cancellation inspected, retried activation or required a completed native turn")
	}
	if _, err = s.Cancel(t.Context(), id, operation); err != nil || local.aborts != 1 {
		t.Fatalf("cancel replay changed stopped incarnation: %v", err)
	}
	if _, err = s.Recover(t.Context(), id); err != nil || local.activations != 1 {
		t.Fatalf("cancel recovery reopened activation: %v", err)
	}
}

func TestPartiallyCreatedInitialSessionCancelsWithoutProvisioning(t *testing.T) {
	t.Parallel()
	s, local, remote, repo := testService(t)
	id, operation := uuid.NewString(), uuid.NewString()
	local.partialCreate = true
	if _, err := s.Start(t.Context(), id, operation, repo, "", nil, nil); err == nil {
		t.Fatal("expected failed initial prepare")
	}
	r, err := s.coordinator.Inspect(id)
	if err != nil || r.Operations[0].Incarnation != nil || r.Phase != session.PhasePreparing {
		t.Fatalf("unexpected partial initial: %+v %v", r, err)
	}
	inspections := local.inspections
	local.failInspect = true
	status, err := s.Cancel(t.Context(), id, uuid.NewString())
	if err != nil || status.Phase != string(session.PhaseCancelled) || status.ThreadID != "" {
		t.Fatalf("partial initial cancellation: %+v %v", status, err)
	}
	if local.initials != 1 || local.activations != 0 || local.inspections != inspections || local.aborts != 1 || remote.prepares != 0 {
		t.Fatal("cancellation repeated preparation or native lifecycle")
	}
}

func TestPreparingInitialSessionRetriesCreateOnlyThroughStart(t *testing.T) {
	t.Parallel()
	s, local, _, repo := testService(t)
	id, operation := uuid.NewString(), uuid.NewString()
	local.partialCreate = true
	if _, err := s.Start(t.Context(), id, operation, repo, "", nil, nil); err == nil {
		t.Fatal("expected failed initial prepare")
	}
	local.partialCreate = false
	status, err := s.Start(t.Context(), id, operation, repo, "", nil, nil)
	if err != nil || status.Phase != string(session.PhaseActive) || local.initials != 2 || local.activations != 1 {
		t.Fatalf("explicit preparation retry: %+v %v", status, err)
	}
}

func TestUnknownInitialUsageNeverStartsRemoteAndCanCancel(t *testing.T) {
	t.Parallel()
	s, local, remote, repo := testService(t)
	id := uuid.NewString()
	if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	local.unknownUsage = true
	if _, err := s.Move(t.Context(), id, uuid.NewString(), testRemote); err == nil {
		t.Fatal("unknown initial usage admitted move")
	}
	if remote.prepares != 0 {
		t.Fatal("unknown usage became zero")
	}
	status, err := s.Cancel(t.Context(), id, uuid.NewString())
	if err != nil || status.Phase != string(session.PhaseCancelled) {
		t.Fatalf("initial cancellation: %+v %v", status, err)
	}
	if _, err = s.Move(t.Context(), id, uuid.NewString(), testRemote); err == nil {
		t.Fatal("cancelled session moved")
	}
}

func TestProfileFolderIsBesideTheConfigurationFile(t *testing.T) {
	t.Parallel()
	dir := testutil.PrivateDir(t)
	path := filepath.Join(dir, "config.json")
	home, err := json.Marshal(filepath.Join(dir, "home"))
	if err != nil {
		t.Fatal(err)
	}
	data := `{"directory":` + string(home) + `,"model":"fixture","local":{"Name":"local"PROFILE},` +
		`"maxDuration":"1h","allowances":{"model.inference":{"units":{"bounded":true,"value":10}}}}`
	if err := testutil.WriteFile(path, []byte(strings.Replace(data, "PROFILE", `,"ProfileDirectory":"/x"`, 1)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(path); err == nil {
		t.Fatal("configuration chose its own profile folder")
	}
	if err := testutil.WriteFile(path, []byte(strings.Replace(data, "PROFILE", "", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := ReadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Local.ProfileDirectory != filepath.Join(dir, ProfileFolder) {
		t.Fatalf("profile folder = %q", c.Local.ProfileDirectory)
	}
}

func TestPrivateConfigRejectsAuthorityFieldsAndSharedFiles(t *testing.T) {
	t.Parallel()
	path := filepath.Join(testutil.PrivateDir(t), "config.json")
	for _, data := range []string{`{"directory":"/tmp/home","model":"fixture","local":{"Name":"local"},"maxDuration":"1h","allowances":{"model.inference":{"units":{"bounded":true,"value":10}}},"authorityFromCheckpoint":true}`, `{} {}`} {
		if err := testutil.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadConfig(path); err == nil {
			t.Fatal("invalid configuration admitted")
		}
	}
	if err := makeTestPathPublic(path); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(path); err == nil {
		t.Fatal("shared administrator file admitted")
	}
}

func TestTerminalInputRetainsPartialBusySuffixAndDetaches(t *testing.T) {
	t.Parallel()
	acks := make(chan terminalFrame, 1)
	var received []byte
	calls := 0
	emit := func(f terminalFrame) error {
		calls++
		n := len(f.Data)
		reply := terminalFrame{Op: terminalWrite}
		switch calls {
		case 1:
			reply.Error = "native terminal input buffer is full"
			n = 0
		case 2:
			n = 2
		}
		received = append(received, f.Data[:n]...)
		reply.Written = n
		acks <- reply
		return nil
	}
	if err := terminalInput(t.Context(), strings.NewReader("abcdef\x1dwithheld"), emit, acks); err != nil {
		t.Fatal(err)
	}
	if string(received) != "abcdef" || calls != 3 {
		t.Fatalf("terminal lost/repeated suffix or sent detach: %q calls=%d", received, calls)
	}
}

// A start that repeats an ID under a new operation is rejected with the
// retained operation, never a hint to try yet another new one.
func TestStartConflictNamesTheRetainedOperation(t *testing.T) {
	t.Parallel()
	s, _, _, repo := testService(t)
	id, operation := uuid.NewString(), uuid.NewString()
	if _, err := s.Start(t.Context(), id, operation, repo, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	_, err := s.Start(t.Context(), id, uuid.NewString(), repo, "", nil, nil)
	want := "home: start intent conflicts with retained launch; " + id + " was started with operation " + operation +
		": to continue it, repeat the original start command with --operation " + operation +
		", or run \"sproozi cancel " + id + "\" to revoke and force-stop its current execution"
	if !errors.Is(err, ErrRejected) || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}

func TestStartSelectsTheRequestedLocalHarnessPerSession(t *testing.T) {
	t.Parallel()
	repo, data, digest := testWorkspace(t)
	codex := &placementFixture{rows: map[string]session.Observation{}, artifact: data, digest: digest, charge: 1}
	claude := &placementFixture{rows: map[string]session.Observation{}, artifact: data, digest: digest, charge: 1}
	dir := filepath.Join(testutil.PrivateDir(t), "home")
	primary := runtimedocker.PlacementConfig{Harness: harness.NativeCodex, Name: testLocal, InstructionDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(testInstructions))), MaxCheckpointBytes: testLimit, RetainedBytes: 64 << 20}
	second := primary
	second.Harness, second.Name = harness.NativeClaude, testClaudeLocal
	placements := map[string]session.Placement{testLocal: codex, testClaudeLocal: claude}
	coordinator, err := session.Open(filepath.Join(dir, "journal"), placements, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	config := Config{Directory: dir, Local: primary, Allowances: map[string]session.Allowance{testCapability: {Units: session.Limit{Bounded: true, Value: 10}}}}
	s := &Service{config: config, duration: time.Hour, stacks: []*localStack{{config: primary}, {config: second}}, placements: placements, coordinator: coordinator}
	t.Cleanup(func() { _ = s.Close() })

	codexID, claudeID := uuid.NewString(), uuid.NewString()
	if _, err = s.Start(t.Context(), codexID, uuid.NewString(), repo, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	claudeOperation := uuid.NewString()
	if _, err = s.Start(t.Context(), claudeID, claudeOperation, repo, harness.NativeClaude, nil, nil); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]runtimedocker.PlacementConfig{codexID: primary, claudeID: second} {
		r, err := s.coordinator.Inspect(id)
		if err != nil {
			t.Fatal(err)
		}
		if launch := r.Operations[0].Launch; launch.Harness != want.Harness || launch.Placement != want.Name {
			t.Fatalf("session %s launched %s on %s", id, launch.Harness, launch.Placement)
		}
		if got := s.localDestination(r, testLocal); got != want.Name {
			t.Fatalf("move --to local for %s resolved to %s", want.Harness, got)
		}
		if got := s.localDestination(r, testRemote); got != testRemote {
			t.Fatalf("remote destination rewritten to %s", got)
		}
	}
	if codex.initials != 1 || claude.initials != 1 {
		t.Fatalf("placements prepared codex=%d claude=%d", codex.initials, claude.initials)
	}
	if _, err = s.Start(t.Context(), claudeID, claudeOperation, repo, harness.NativeCodex, nil, nil); err == nil {
		t.Fatal("retry changed an existing session's harness")
	}
	single, _, _, _ := testService(t)
	if _, err = single.Start(t.Context(), uuid.NewString(), uuid.NewString(), repo, harness.NativeClaude, nil, nil); err == nil || !strings.Contains(err.Error(), "not configured") {
		t.Fatalf("unconfigured harness: %v", err)
	}
}

// Harness arguments belong to the execution: a refused flag never creates one,
// a retry must repeat them, and they keep the execution at home.
func TestStartHarnessArgsAreCheckedAndKeptWithTheExecution(t *testing.T) {
	t.Parallel()
	s, _, _, repo := testService(t)
	id, operation := uuid.NewString(), uuid.NewString()
	_, err := s.Start(t.Context(), id, operation, repo, "", nil, []string{"--sandbox", "read-only"})
	if !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "--sandbox") {
		t.Fatalf("refused harness flag: %v", err)
	}
	if _, inspectErr := s.coordinator.Inspect(id); inspectErr == nil {
		t.Fatal("refused harness arguments created an execution")
	}
	args := []string{"--model", "gpt-5.5"}
	status, err := s.Start(t.Context(), id, operation, repo, "", nil, args)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(status.Session.HarnessArgs, args) || !slices.Equal(status.Session.Active.Launch.HarnessArgs, args) {
		t.Fatalf("harness arguments not kept: %+v", status.Session)
	}
	if _, err = s.Start(t.Context(), id, operation, repo, "", nil, nil); !errors.Is(err, ErrRejected) {
		t.Fatalf("retry with other harness arguments: %v", err)
	}
	_, err = s.Move(t.Context(), id, uuid.NewString(), testRemote)
	if !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), "harness arguments") {
		t.Fatalf("remote move with harness arguments: %v", err)
	}
}

// A workspace start cannot capture is refused before anything is recorded,
// with the reason and the repair, so the client offers no retry.
func TestStartRejectsWorkspaceProblemsWithRepair(t *testing.T) {
	t.Parallel()
	s, _, _, repo := testService(t)
	plain := testutil.PrivateDir(t)
	for name, tc := range map[string]struct {
		path  string
		limit int64
		want  string
	}{
		"missing":   {filepath.Join(plain, "missing"), testLimit, "does not exist; pass --workspace"},
		"not git":   {plain, testLimit, "must be the top-level directory of a Git repository"},
		"too large": {repo, 1024, "local.maxCheckpointBytes is 0.0 MiB: raise it"},
	} {
		s.config.Local.MaxCheckpointBytes = tc.limit
		id := uuid.NewString()
		_, err := s.Start(t.Context(), id, uuid.NewString(), tc.path, "", nil, nil)
		if !errors.Is(err, ErrRejected) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v", name, err)
		}
		if _, inspectErr := s.coordinator.Inspect(id); !os.IsNotExist(inspectErr) {
			t.Errorf("%s: execution recorded: %v", name, inspectErr)
		}
	}
}
