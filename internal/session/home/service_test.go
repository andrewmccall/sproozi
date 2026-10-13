package home

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
const testIgnoredFile = "needed.bin"

type placementFixture struct {
	rows                                           map[string]session.Observation
	artifact                                       []byte
	digest                                         string
	initials, prepares, activations, stops, aborts int
	lostStop, lostActivate, unknownUsage           bool
	rejectActivate, failInspect                    bool
	partialCreate                                  bool
	inspections                                    int
	charge                                         int64
}

func (p *placementFixture) prepare(l session.Launch) (session.Incarnation, error) {
	if r, ok := p.rows[l.AllocationID]; ok && r.State != session.PlacementPreparing {
		return r.Incarnation, nil
	}
	i := session.Incarnation{ExecutionID: l.AllocationID, ResourceUID: uuid.NewString()}
	p.rows[l.AllocationID] = session.Observation{Launch: l, Incarnation: i, State: session.PlacementPrepared}
	return i, nil
}
func (p *placementFixture) CreateLocal(_ context.Context, l session.Launch, _ []byte) (session.Incarnation, error) {
	if l.ThreadID != "" || l.CheckpointDigest != "" {
		return session.Incarnation{}, errors.New("initial session fabricated native state")
	}
	p.initials++
	if p.partialCreate {
		p.rows[l.AllocationID] = session.Observation{Launch: l, State: session.PlacementPreparing}
		return session.Incarnation{}, errors.New("partial initial preparation")
	}
	return p.prepare(l)
}
func (p *placementFixture) Prepare(_ context.Context, l session.Launch, data []byte) (session.Incarnation, error) {
	if l.ThreadID != testNativeUUID || l.CheckpointDigest != p.digest || string(data) != string(p.artifact) {
		return session.Incarnation{}, errors.New("native continuity changed")
	}
	p.prepares++
	return p.prepare(l)
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
func (p *placementFixture) Stop(_ context.Context, l session.Launch, i session.Incarnation) (session.Stopped, error) {
	r := p.rows[l.AllocationID]
	if r.Incarnation != i {
		return session.Stopped{}, errors.New("wrong stop incarnation")
	}
	if r.Stopped != nil {
		return *r.Stopped, nil
	}
	p.stops++
	l.ThreadID = testNativeUUID
	stopped := session.Stopped{Launch: l, Incarnation: i, ActualStopped: true, Checkpoint: p.artifact, CheckpointDigest: p.digest, Receipt: &session.Receipt{Launch: l, Incarnation: i, Usage: map[string]budget.TrustedUsage{testCapability: {Units: p.charge}}}}
	if p.unknownUsage {
		stopped.Receipt = nil
	}
	r.State = session.PlacementStopped
	r.Stopped = &stopped
	r.Launch = l
	p.rows[l.AllocationID] = r
	if p.lostStop {
		p.lostStop = false
		return session.Stopped{}, errors.New("lost stop reply")
	}
	return stopped, nil
}
func (p *placementFixture) Abort(_ context.Context, l session.Launch, i session.Incarnation) (session.Observation, error) {
	p.aborts++
	r := p.rows[l.AllocationID]
	if r.Incarnation != i {
		return r, errors.New("wrong abort incarnation")
	}
	r.State = session.PlacementStopped
	r.Stopped = &session.Stopped{Launch: l, Incarnation: i, ActualStopped: true}
	p.rows[l.AllocationID] = r
	return r, nil
}
func (p *placementFixture) CancelInitial(ctx context.Context, l session.Launch) error {
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

func testWorkspace(t *testing.T) string {
	t.Helper()
	repo, home := t.TempDir(), t.TempDir()
	for _, args := range [][]string{{"init", "--quiet", "--template=", "-b", "main"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@invalid", "commit", "--quiet", "--allow-empty", "-m", "Fixture"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("Git fixture: %v %s", err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "unfinished.txt"), []byte("unfinished work\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return repo
}

// Synthetic native files are unit fixtures only; the production verifier owns
// the empirical stock-client proof and cannot use these as mobility evidence.
func testArtifact(t *testing.T, repo string, includeIgnored ...string) ([]byte, string) {
	t.Helper()
	home := t.TempDir()
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
	repo := testWorkspace(t)
	data, digest := testArtifact(t, repo)
	local := &placementFixture{rows: map[string]session.Observation{}, artifact: data, digest: digest, charge: 2}
	remote := &placementFixture{rows: map[string]session.Observation{}, artifact: data, digest: digest, charge: 3}
	dir := filepath.Join(t.TempDir(), "home")
	initialDir := filepath.Join(dir, "initial")
	if err := os.MkdirAll(initialDir, 0700); err != nil {
		t.Fatal(err)
	}
	config := Config{Directory: dir, Local: runtimedocker.PlacementConfig{Harness: harness.NativeCodex, Name: testLocal, InstructionDigest: fmt.Sprintf("%x", sha256.Sum256([]byte(testInstructions))), MaxCheckpointBytes: testLimit, RetainedBytes: 64 << 20}, Allowances: map[string]session.Allowance{testCapability: {Units: session.Limit{Bounded: true, Value: 10}}}}
	placements := map[string]session.Placement{testLocal: local, testRemote: remote}
	coordinator, err := session.Open(filepath.Join(dir, "journal"), placements, testLimit)
	if err != nil {
		t.Fatal(err)
	}
	s := &Service{config: config, duration: time.Hour, initialDir: initialDir, local: local, placements: placements, coordinator: coordinator}
	t.Cleanup(func() { _ = s.Close() })
	return s, local, remote, repo
}

func TestStartIgnoredOptInRetainsBytesAndImmutableSelectionAcrossVisits(t *testing.T) {
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
	if _, err := s.Start(t.Context(), id, operation, repo, []string{testIgnoredFile}); err != nil {
		t.Fatal(err)
	}
	initial, err := s.readInitial(id)
	if err != nil {
		t.Fatal(err)
	}
	input, err := os.ReadFile(filepath.Join(s.initialDir, id+".workspace"))
	if err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(t.TempDir(), "restored")
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
	if len(initial.Launch.IncludeIgnored) != 1 || initial.Launch.IncludeIgnored[0] != testIgnoredFile {
		t.Fatal("initial launch omitted the approved ignored file")
	}
	if _, err = s.Start(t.Context(), id, operation, repo, nil); err == nil {
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
	s, local, remote, repo := testService(t)
	id, startOp, moveOp := uuid.NewString(), uuid.NewString(), uuid.NewString()
	local.lostActivate = true
	if _, err := s.Start(t.Context(), id, startOp, repo, nil); err == nil {
		t.Fatal("lost activation reply succeeded")
	}
	status, err := s.Recover(t.Context(), id)
	if err != nil || status.InitialStage != initialActive || status.ThreadID != "" {
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

func TestRecoverImportBeforeFirstMoveWasJournaled(t *testing.T) {
	s, _, remote, repo := testService(t)
	id := uuid.NewString()
	if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, nil); err != nil {
		t.Fatal(err)
	}
	r, err := s.readInitial(id)
	if err != nil {
		t.Fatal(err)
	}
	r.Stage = initialImporting
	r.OperationID = uuid.NewString()
	r.Destination = testRemote
	if err = s.saveInitial(r); err != nil {
		t.Fatal(err)
	}
	if _, err = s.importInitial(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	// Emulate restart after durable import and before Coordinator.Move saved.
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
	s, local, _, repo := testService(t)
	id := uuid.NewString()
	local.rejectActivate = true
	if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, nil); err == nil {
		t.Fatal("native activation rejection hidden")
	}
	retained, err := s.readInitial(id)
	if err != nil || retained.Stage != initialPreparing || retained.Incarnation == nil {
		t.Fatalf("failed activation lost exact incarnation: %+v %v", retained, err)
	}
	local.failInspect = true
	inspections := local.inspections
	operation := uuid.NewString()
	status, err := s.Cancel(t.Context(), id, operation)
	if err != nil || status.InitialStage != initialCancelled {
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
	s, local, remote, repo := testService(t)
	id, operation := uuid.NewString(), uuid.NewString()
	local.partialCreate = true
	if _, err := s.Start(t.Context(), id, operation, repo, nil); err == nil {
		t.Fatal("expected failed initial prepare")
	}
	r, err := s.readInitial(id)
	if err != nil || r.Incarnation != nil || r.Stage != initialPreparing {
		t.Fatalf("unexpected partial initial: %+v %v", r, err)
	}
	inspections := local.inspections
	local.failInspect = true
	status, err := s.Cancel(t.Context(), id, uuid.NewString())
	if err != nil || status.InitialStage != initialCancelled || status.ThreadID != "" {
		t.Fatalf("partial initial cancellation: %+v %v", status, err)
	}
	if local.initials != 1 || local.activations != 0 || local.inspections != inspections || local.aborts != 1 || remote.prepares != 0 {
		t.Fatal("cancellation repeated preparation or native lifecycle")
	}
}

func TestPreparingInitialSessionRetriesCreateOnlyThroughStart(t *testing.T) {
	s, local, _, repo := testService(t)
	id, operation := uuid.NewString(), uuid.NewString()
	local.partialCreate = true
	if _, err := s.Start(t.Context(), id, operation, repo, nil); err == nil {
		t.Fatal("expected failed initial prepare")
	}
	local.partialCreate = false
	status, err := s.Start(t.Context(), id, operation, repo, nil)
	if err != nil || status.InitialStage != initialActive || local.initials != 2 || local.activations != 1 {
		t.Fatalf("explicit preparation retry: %+v %v", status, err)
	}
}

func TestUnknownInitialUsageNeverStartsRemoteAndCanCancel(t *testing.T) {
	s, local, remote, repo := testService(t)
	id := uuid.NewString()
	if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, nil); err != nil {
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
	if err != nil || status.InitialStage != initialCancelled {
		t.Fatalf("initial cancellation: %+v %v", status, err)
	}
	if _, err = s.Move(t.Context(), id, uuid.NewString(), testRemote); err == nil {
		t.Fatal("cancelled session moved")
	}
}

func TestPrivateConfigRejectsAuthorityFieldsAndSharedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	for _, data := range []string{`{"directory":"/tmp/home","model":"fixture","local":{"Name":"local"},"maxDuration":"1h","allowances":{"model.inference":{"units":{"bounded":true,"value":10}}},"authorityFromCheckpoint":true}`, `{} {}`} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadConfig(path); err == nil {
			t.Fatal("invalid configuration admitted")
		}
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(path); err == nil {
		t.Fatal("shared administrator file admitted")
	}
}

func TestTerminalInputRetainsPartialBusySuffixAndDetaches(t *testing.T) {
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
