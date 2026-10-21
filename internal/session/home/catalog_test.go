package home

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/testutil"

	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/work"
	"github.com/google/uuid"
)

type workSourceFixture struct {
	export    work.Export
	begins    int
	claims    int
	lostClaim bool
	denyClaim bool
}

func (f *workSourceFixture) BeginExport(_ context.Context, id, operation string) (work.Export, error) {
	f.begins++
	if id != f.export.ID || operation != f.export.OperationID {
		return work.Export{}, errors.New("export identity conflicts")
	}
	return f.export, nil
}

func (f *workSourceFixture) Claim(_ context.Context, id, operation, digest string) (work.Export, error) {
	f.claims++
	if f.denyClaim || id != f.export.ID || operation != f.export.OperationID || digest != f.export.Stopped.CheckpointDigest {
		return work.Export{}, errors.New("claim denied")
	}
	f.export.Claimed = true
	if f.lostClaim {
		f.lostClaim = false
		return work.Export{}, errors.New("claim committed but response lost")
	}
	return f.export, nil
}

func importedFixture(t *testing.T) (*Service, *placementFixture, *workSourceFixture) {
	t.Helper()
	s, local, _, _ := testService(t)
	id, operation := uuid.NewString(), uuid.NewString()
	launch := session.Launch{Harness: s.config.Local.Harness, ExecutionID: uuid.NewString(),
		OperationID: uuid.NewString(), AllocationID: uuid.NewString(), Epoch: 1, Placement: testRemote,
		ThreadID: testNativeUUID, InstructionDigest: s.config.Local.InstructionDigest,
		Deadline:   time.Now().Add(-time.Hour).UTC(),
		Allowances: map[string]session.Allowance{testCapability: {Units: session.Limit{Bounded: true, Value: 20}}}}
	incarnation := session.Incarnation{ExecutionID: uuid.NewString(), ResourceUID: uuid.NewString()}
	stopped := session.Stopped{Launch: launch, Incarnation: incarnation, ActualStopped: true,
		CheckpointDigest: local.digest,
		Receipt: &session.Receipt{Launch: launch, Incarnation: incarnation,
			Usage: map[string]budget.TrustedUsage{testCapability: {Units: 9}}}}
	source := &workSourceFixture{export: work.Export{ID: id, OperationID: operation, Harness: launch.Harness,
		Stopped: stopped, Checkpoint: local.artifact}}
	s.SetWorkSources(map[string]WorkSource{testRemote: source})
	return s, local, source
}

func TestPullClaimReplayAndFreshResumePreserveSourceAccounts(t *testing.T) {
	t.Parallel()
	s, local, source := importedFixture(t)
	id, operation := source.export.ID, source.export.OperationID
	source.lostClaim = true
	if _, err := s.Pull(t.Context(), id, operation, testRemote); err == nil {
		t.Fatal("lost claim response became acknowledged ownership")
	}
	if _, err := s.Resume(t.Context(), id, uuid.NewString(), nil); err == nil || local.prepares != 0 {
		t.Fatal("downloaded bytes acquired activation authority")
	}
	// A retry with a fresh operation and no source continues the retained pull.
	status, err := s.Pull(t.Context(), id, uuid.NewString(), "")
	if err != nil || source.begins != 1 || source.claims != 2 || status.InitialStage != stageRetained || status.ThreadID != testNativeUUID {
		t.Fatalf("claim replay: %+v %v, begin=%d claim=%d", status, err, source.begins, source.claims)
	}
	assertFreshResume(t, s, local, source, id)
}

func assertFreshResume(t *testing.T, s *Service, local *placementFixture, source *workSourceFixture, id string) {
	t.Helper()
	before, _ := json.Marshal(source.export.Stopped.Receipt)
	resume := uuid.NewString()
	status, err := s.Resume(t.Context(), id, resume, nil)
	if err != nil || status.ID != id || status.ExecutionID == id || status.Phase != string(session.PhaseActive) || status.ThreadID != testNativeUUID {
		t.Fatalf("fresh native resume: %+v %v", status, err)
	}
	r, err := s.coordinator.Inspect(status.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	first := r.Operations[0].Launch
	if first.CheckpointDigest != source.export.Stopped.CheckpointDigest || first.Allowances[testCapability].Units.Value != 10 || !first.Deadline.After(time.Now()) || first.AllocationID == source.export.Stopped.Launch.AllocationID {
		t.Fatalf("new admission credited source authority: %+v", r)
	}
	if len(r.Visits) != 0 || !reflect.DeepEqual(r.Remaining, r.Ceiling) {
		t.Fatalf("resume invented a previous source receipt in its coordinator: %+v", r)
	}
	replayed, err := s.Resume(t.Context(), id, resume, nil)
	if err != nil || replayed.ID != id || replayed.ExecutionID != status.ExecutionID || local.activations != 1 || local.initials != 0 || local.prepares != 1 {
		t.Fatalf("resume replay: %+v %v, activations=%d", replayed, err, local.activations)
	}
	if _, err = s.Resume(t.Context(), id, uuid.NewString(), nil); err == nil {
		t.Fatal("active prior writer admitted a new envelope")
	}
	listed, err := s.Sessions(work.SessionQuery{})
	if err != nil || len(listed) != 1 || listed[0].ID != id || listed[0].ExecutionID != status.ExecutionID || listed[0].Session != nil {
		t.Fatalf("session catalogue duplicated internal execution: %+v %v", listed, err)
	}
	assertCompletedResumeCycle(t, s, local, source, status, before)
}

func assertCompletedResumeCycle(t *testing.T, s *Service, local *placementFixture, source *workSourceFixture, status Status, before []byte) {
	t.Helper()
	id := source.export.ID
	stopOperation := uuid.NewString()
	stopped, err := s.Stop(t.Context(), id, stopOperation)
	if err != nil || stopped.ID != id || stopped.ExecutionID != status.ExecutionID || stopped.AllocationID != status.AllocationID || stopped.Incarnation == nil || stopped.InitialStage != stageRetained || stopped.ThreadID != testNativeUUID {
		t.Fatalf("stopped session checkpoint: %+v %v", stopped, err)
	}
	next, err := s.Resume(t.Context(), id, uuid.NewString(), nil)
	if err != nil || next.ID != id || next.ExecutionID == status.ExecutionID || local.activations != 2 {
		t.Fatalf("separate later envelope: %+v %v", next, err)
	}
	if _, err = s.Stop(t.Context(), id, stopOperation); err == nil {
		t.Fatal("delayed stop request stopped a later execution")
	}
	if _, err = s.Cancel(t.Context(), id, stopOperation); err == nil {
		t.Fatal("old stop operation acquired cancellation authority")
	}
	if _, err = s.Move(t.Context(), id, stopOperation, testRemote); err == nil {
		t.Fatal("old stop operation moved a later execution")
	}
	active, err := s.Status(id)
	if err != nil || active.ID != id || active.ExecutionID != next.ExecutionID || active.Phase != string(session.PhaseActive) {
		t.Fatalf("replay affected later writer: %+v %v", active, err)
	}
	after, _ := json.Marshal(source.export.Stopped.Receipt)
	if string(before) != string(after) {
		t.Fatal("local execution rewrote remote sealed accounts")
	}
	old, err := s.coordinator.Inspect(status.ExecutionID)
	if err != nil || len(old.Visits) != 1 || old.Visits[0].Receipt.Usage[testCapability].Units != 2 {
		t.Fatalf("previous local spent accounting unavailable: %+v %v", old, err)
	}
}

func TestPullRejectsUnknownSourceAndChangedClaim(t *testing.T) {
	t.Parallel()
	for _, scenario := range []string{"writer", "accounting", "native", "artifact", "unacknowledged"} {
		t.Run(scenario, func(t *testing.T) {
			s, local, source := importedFixture(t)
			switch scenario {
			case "writer":
				source.export.Stopped.ActualStopped = false
			case "accounting":
				source.export.Stopped.Receipt = nil
			case "native":
				source.export.Stopped.Launch.ThreadID = uuid.NewString()
			case "artifact":
				source.export.Checkpoint = []byte("invalid checkpoint")
			case "unacknowledged":
				source.denyClaim = true
			}
			if _, err := s.Pull(t.Context(), source.export.ID, source.export.OperationID, testRemote); err == nil {
				t.Fatal("invalid source acquired ownership")
			}
			if _, err := s.Resume(t.Context(), source.export.ID, uuid.NewString(), nil); err == nil || local.activations != 0 {
				t.Fatal("invalid source activated local writer")
			}
		})
	}
}

func TestImportedResumeLostActivationAndUnknownStopKeepOneWriter(t *testing.T) {
	t.Parallel()
	s, local, source := importedFixture(t)
	id := source.export.ID
	if _, err := s.Pull(t.Context(), id, source.export.OperationID, testRemote); err != nil {
		t.Fatal(err)
	}
	op := uuid.NewString()
	local.lostActivate = true
	if _, err := s.Resume(t.Context(), id, op, nil); err == nil {
		t.Fatal("lost activation response was successful")
	}
	status, err := s.Resume(t.Context(), id, op, nil)
	if err != nil || local.activations != 1 {
		t.Fatalf("lost activation repeated native launch: %+v %v", status, err)
	}
	local.unknownUsage = true
	if _, err := s.Stop(t.Context(), id, uuid.NewString()); err == nil {
		t.Fatal("missing local accounting became a committed checkpoint")
	}
	if _, err := s.Resume(t.Context(), id, uuid.NewString(), nil); err == nil || local.activations != 1 {
		t.Fatal("unknown stopped source admitted another writer")
	}
	r, err := s.readCatalog(id)
	if err != nil || !reflect.DeepEqual(r.Committed.Receipt, source.export.Stopped.Receipt) {
		t.Fatalf("failed capture replaced prior revision: %+v %v", r, err)
	}
}

func TestStopMovedProductFinalizesWithoutDestinationAndRemainsReplayable(t *testing.T) {
	t.Parallel()
	s, local, source := importedFixture(t)
	remote := s.placements[testRemote].(*placementFixture)
	id := source.export.ID
	if _, err := s.Pull(t.Context(), id, source.export.OperationID, testRemote); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resume(t.Context(), id, uuid.NewString(), nil); err != nil {
		t.Fatal(err)
	}
	moveOperation := uuid.NewString()
	if _, err := s.Move(t.Context(), id, moveOperation, testRemote); err != nil {
		t.Fatal(err)
	}
	prepares := local.prepares + remote.prepares
	status, err := s.Stop(t.Context(), id, uuid.NewString())
	if err != nil || status.InitialStage != stageRetained || prepares != local.prepares+remote.prepares {
		t.Fatalf("stop activated destination: %+v %v", status, err)
	}
	if _, err = s.Resume(t.Context(), id, uuid.NewString(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Move(t.Context(), id, moveOperation, testRemote); err == nil {
		t.Fatal("old move operation acquired a later execution")
	}
}

func TestIdleStopReplayCannotStopTheFirstContinuation(t *testing.T) {
	t.Parallel()
	s, _, source := importedFixture(t)
	id := source.export.ID
	if _, err := s.Pull(t.Context(), id, source.export.OperationID, testRemote); err != nil {
		t.Fatal(err)
	}
	operation := uuid.NewString()
	if _, err := s.Stop(t.Context(), id, operation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resume(t.Context(), id, uuid.NewString(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stop(t.Context(), id, operation); err == nil {
		t.Fatal("idle stop replay acquired a later writer")
	}
}

func TestPullRetentionRejectionDoesNotClaimOrActivateTheSource(t *testing.T) {
	t.Parallel()
	s, local, source := importedFixture(t)
	s.config.Local.RetainedBytes = int64(len(source.export.Checkpoint)) - 1
	id, operation := source.export.ID, source.export.OperationID
	if _, err := s.Pull(t.Context(), id, operation, testRemote); err == nil {
		t.Fatal("oversized retained catalogue admitted")
	}
	if source.claims != 0 || local.prepares != 0 || local.activations != 0 {
		t.Fatal("storage rejection claimed or activated a writer")
	}
	s.config.Local.RetainedBytes = 64 << 20
	if _, err := s.Pull(t.Context(), id, operation, testRemote); err != nil {
		t.Fatal("original transfer did not recover after capacity was restored", err)
	}
}

func TestFreshResumeHonorsExistingExecutionRetentionBounds(t *testing.T) {
	t.Parallel()
	s, local, source := importedFixture(t)
	id := source.export.ID
	if _, err := s.Pull(t.Context(), id, source.export.OperationID, testRemote); err != nil {
		t.Fatal(err)
	}
	s.config.Local.RetainedBytes = s.config.Local.MaxCheckpointBytes + 1
	if err := testutil.WriteFile(filepath.Join(s.config.Directory, "journal", "retained-padding"), []byte("xx"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resume(t.Context(), id, uuid.NewString(), nil); err == nil {
		t.Fatal("fresh execution bypassed retention bound")
	}
	if local.prepares != 0 || local.activations != 0 {
		t.Fatal("storage rejection admitted a writer")
	}
}

// exchange sends one control request through handle and decodes its reply.
func exchange(t *testing.T, s *Service, q Request) Response {
	t.Helper()
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	go s.handle(t.Context(), server)
	if err := json.NewEncoder(client).Encode(q); err != nil {
		t.Fatal(err)
	}
	var r Response
	if err := json.NewDecoder(client).Decode(&r); err != nil {
		t.Fatal(err)
	}
	return r
}

// An unknown session UUID gets an actionable reply on every session command,
// never the journal's file path.
func TestUnknownSessionRepliesWithoutJournalPath(t *testing.T) {
	t.Parallel()
	s, _, _, _ := testService(t)
	id := uuid.NewString()
	want := "docker (home): no session " + id + `; "sproozi ls" lists sessions`
	for _, q := range []Request{
		{Action: ActionStatus, ID: id},
		{Action: ActionStop, ID: id, OperationID: uuid.NewString()},
		{Action: ActionCancel, ID: id, OperationID: uuid.NewString()},
		{Action: ActionRecover, ID: id},
		{Action: ActionMove, ID: id, OperationID: uuid.NewString(), Destination: testRemote},
		{Action: ActionResume, ID: id, OperationID: uuid.NewString()},
		{Action: ActionAttach, ID: id},
	} {
		r := exchange(t, s, q)
		if r.Error != want || !r.UnknownSession {
			t.Errorf("%s replied %+v, want %q", q.Action, r, want)
		}
		if err := replyError(r); !errors.Is(err, ErrUnknownSession) || err.Error() != want {
			t.Errorf("%s reply error %v", q.Action, err)
		}
	}
}

// A session created by start resumes where it stopped: a new execution from
// its last checkpoint, with the same conversation, fresh limits and a new
// deadline, even after the first execution's deadline.
func TestResumeContinuesAStartedSessionAfterItsDeadline(t *testing.T) {
	t.Parallel()
	s, local, _, repo := testService(t)
	id := uuid.NewString()
	s.duration = 5 * time.Second
	if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	r := exchange(t, s, Request{Action: ActionResume, ID: id, OperationID: uuid.NewString()})
	if !r.Rejected || !strings.Contains(r.Error, `run "sproozi stop `+id+`" first`) {
		t.Fatalf("resume of a running session replied %+v", r)
	}
	stopped, err := s.Stop(t.Context(), id, uuid.NewString())
	if err != nil || stopped.Phase != stageRetained || stopped.ThreadID != testNativeUUID {
		t.Fatalf("stop of a started session: %+v %v", stopped, err)
	}
	time.Sleep(time.Until(stopped.Deadline) + 100*time.Millisecond)
	s.duration = time.Hour
	resumed, err := s.Resume(t.Context(), id, uuid.NewString(), []string{"--search"})
	if err != nil {
		t.Fatal(err)
	}
	active := resumed.Session.Active
	if resumed.ID != id || resumed.ExecutionID == id || active == nil || active.Launch.ThreadID != testNativeUUID ||
		active.Launch.CheckpointDigest != local.digest || !active.Launch.Deadline.After(time.Now().Add(time.Minute)) ||
		!slices.Equal(active.Launch.HarnessArgs, []string{"--search"}) {
		t.Fatalf("resumed session: %+v", resumed)
	}
	if again, err := s.Stop(t.Context(), id, uuid.NewString()); err != nil || again.ThreadID != testNativeUUID {
		t.Fatalf("stop of the resumed execution: %+v %v", again, err)
	}
}

// A session stopped with no conversation resumes from the workspace it
// started from, as a new conversation.
func TestResumeOfTurnlessSessionStartsFromItsWorkspace(t *testing.T) {
	t.Parallel()
	s, local, _, repo := testService(t)
	local.turnless = true
	id := uuid.NewString()
	if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	stopped, err := s.Stop(t.Context(), id, uuid.NewString())
	if err != nil || stopped.Phase != stageRetained || stopped.ThreadID != "" {
		t.Fatalf("turnless stop: %+v %v", stopped, err)
	}
	resumed, err := s.Resume(t.Context(), id, uuid.NewString(), nil)
	if err != nil {
		t.Fatal(err)
	}
	active := resumed.Session.Active
	if active == nil || active.Launch.ThreadID != "" || active.Launch.CheckpointDigest != "" || resumed.Session.Workspace == "" {
		t.Fatalf("turnless resume: %+v", resumed.Session)
	}
}

// Pull names an executor this service has not reached, and says how to reach it.
func TestPullNamesUnknownRemoteExecutor(t *testing.T) {
	t.Parallel()
	s, _, source := importedFixture(t)
	_, err := s.Pull(t.Context(), source.export.ID, source.export.OperationID, "elsewhere")
	if err == nil || err.Error() != `home: no remote executor named "elsewhere" is known to this service; `+
		`reach it with --context` || !errors.Is(err, ErrRejected) {
		t.Fatalf("pull from an unknown executor: %v", err)
	}
	_, err = s.Pull(t.Context(), source.export.ID, source.export.OperationID, "")
	if err == nil || !strings.Contains(err.Error(), "pass --context") {
		t.Fatalf("pull without a source: %v", err)
	}
}

// Move refuses an unknown destination with the commands that choose one.
func TestMoveNamesUnconfiguredDestination(t *testing.T) {
	t.Parallel()
	s, _, _, repo := testService(t)
	id := uuid.NewString()
	if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ operation, destination, want string }{
		{uuid.NewString(), "docker", `home: this service cannot move a session to "docker"; run ` +
			`"sproozi move ID --local" to bring it to this machine, or "sproozi move ID --remote" ` +
			`(add --context for another cluster)`},
	} {
		r := exchange(t, s, Request{Action: ActionMove, ID: id, OperationID: tc.operation, Destination: tc.destination})
		if r.Error != tc.want || !r.Rejected || !errors.Is(replyError(r), ErrRejected) {
			t.Errorf("move replied %+v, want rejected %q", r, tc.want)
		}
	}
}

// rm refuses a running session, then deletes a stopped one with every
// journal and retained file only it used; the listing no longer shows it.
func TestRemoveDeletesAStoppedSessionAndItsFiles(t *testing.T) {
	t.Parallel()
	s, _, _, repo := testService(t)
	id := uuid.NewString()
	if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	r := exchange(t, s, Request{Action: ActionRemove, ID: id})
	if !r.Rejected || !strings.Contains(r.Error, `stop it with "sproozi stop `+id+`"`) {
		t.Fatalf("remove of a running session replied %+v", r)
	}
	if _, err := s.Stop(t.Context(), id, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resume(t.Context(), id, uuid.NewString(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stop(t.Context(), id, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(s.config.Directory, "journal")
	before := countFiles(t, journal)
	r = exchange(t, s, Request{Action: ActionRemove, ID: id})
	if r.Error != "" || r.Status == nil || r.Status.ID != id {
		t.Fatalf("remove replied %+v", r)
	}
	if after := countFiles(t, journal); after >= before || after != 0 {
		t.Fatalf("journal files before %d, after %d", before, after)
	}
	if listed, err := s.Sessions(work.SessionQuery{}); err != nil || len(listed) != 0 {
		t.Fatalf("removed session still listed: %+v %v", listed, err)
	}
	if r = exchange(t, s, Request{Action: ActionRemove, ID: id}); !r.UnknownSession {
		t.Fatalf("second remove replied %+v", r)
	}
}

func countFiles(t *testing.T, dir string) int {
	t.Helper()
	count := 0
	err := filepath.WalkDir(dir, func(_ string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && entry.Name() != "owner.lock" {
			count++
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return count
}

// "local" names this machine's placement in a move, whatever it is called.
func TestMoveToLocalAliasReturnsToThisMachine(t *testing.T) {
	t.Parallel()
	s, _, _, repo := testService(t)
	id := uuid.NewString()
	if _, err := s.Start(t.Context(), id, uuid.NewString(), repo, "", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Move(t.Context(), id, uuid.NewString(), testRemote); err != nil {
		t.Fatal(err)
	}
	status, err := s.Move(t.Context(), id, uuid.NewString(), LocalAlias)
	if err != nil || status.Placement != s.config.Local.Name {
		t.Fatalf("move to local: %+v %v", status, err)
	}
}
