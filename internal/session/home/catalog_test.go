package home

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

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
	launch := session.Launch{Harness: s.config.Local.Harness, SessionID: uuid.NewString(),
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
	s, local, source := importedFixture(t)
	id, operation := source.export.ID, source.export.OperationID
	source.lostClaim = true
	if _, err := s.Pull(t.Context(), id, operation, testRemote); err == nil {
		t.Fatal("lost claim response became acknowledged ownership")
	}
	if _, err := s.Resume(t.Context(), id, uuid.NewString()); err == nil || local.prepares != 0 {
		t.Fatal("downloaded bytes acquired activation authority")
	}
	status, err := s.Pull(t.Context(), id, operation, testRemote)
	if err != nil || source.begins != 1 || source.claims != 2 || status.InitialStage != stageRetained || status.ThreadID != testNativeUUID {
		t.Fatalf("claim replay: %+v %v, begin=%d claim=%d", status, err, source.begins, source.claims)
	}
	assertFreshResume(t, s, local, source, id)
}

func assertFreshResume(t *testing.T, s *Service, local *placementFixture, source *workSourceFixture, id string) {
	t.Helper()
	before, _ := json.Marshal(source.export.Stopped.Receipt)
	resume := uuid.NewString()
	status, err := s.Resume(t.Context(), id, resume)
	if err != nil || status.ProductID != id || status.ID == id || status.InitialStage != "active" || status.ThreadID != testNativeUUID {
		t.Fatalf("fresh native resume: %+v %v", status, err)
	}
	r, err := s.readInitial(status.ID)
	if err != nil || r.Input != checkpointInput || r.Launch.Allowances[testCapability].Units.Value != 10 || !r.Launch.Deadline.After(time.Now()) || r.Launch.AllocationID == source.export.Stopped.Launch.AllocationID {
		t.Fatalf("new admission credited source authority: %+v %v", r, err)
	}
	if _, err = os.Stat(filepath.Join(s.config.Directory, "journal", status.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("resume invented a previous source receipt in its coordinator")
	}
	replayed, err := s.Resume(t.Context(), id, resume)
	if err != nil || replayed.ID != status.ID || local.activations != 1 || local.initials != 0 || local.prepares != 1 {
		t.Fatalf("resume replay: %+v %v, activations=%d", replayed, err, local.activations)
	}
	if _, err = s.Resume(t.Context(), id, uuid.NewString()); err == nil {
		t.Fatal("active prior writer admitted a new envelope")
	}
	listed, err := s.Sessions(work.SessionQuery{})
	if err != nil || len(listed) != 1 || listed[0].ProductID != id {
		t.Fatalf("product catalog duplicated internal execution: %+v %v", listed, err)
	}
	assertCompletedResumeCycle(t, s, local, source, status, before)
}

func assertCompletedResumeCycle(t *testing.T, s *Service, local *placementFixture, source *workSourceFixture, status Status, before []byte) {
	t.Helper()
	id := source.export.ID
	stopOperation := uuid.NewString()
	stopped, err := s.Stop(t.Context(), id, stopOperation)
	if err != nil || stopped.InitialStage != stageRetained || stopped.ThreadID != testNativeUUID {
		t.Fatalf("stopped product checkpoint: %+v %v", stopped, err)
	}
	next, err := s.Resume(t.Context(), id, uuid.NewString())
	if err != nil || next.ID == status.ID || local.activations != 2 {
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
	if err != nil || active.ID != next.ID || active.InitialStage != initialActive {
		t.Fatalf("replay affected later writer: %+v %v", active, err)
	}
	after, _ := json.Marshal(source.export.Stopped.Receipt)
	if string(before) != string(after) {
		t.Fatal("local execution rewrote remote sealed accounts")
	}
	old, err := s.readInitial(status.ID)
	if err != nil || old.Stopped == nil || old.Stopped.Receipt.Usage[testCapability].Units != 2 {
		t.Fatalf("previous local spent accounting unavailable: %+v %v", old, err)
	}
}

func TestPullRejectsUnknownSourceAndChangedClaim(t *testing.T) {
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
			if _, err := s.Resume(t.Context(), source.export.ID, uuid.NewString()); err == nil || local.activations != 0 {
				t.Fatal("invalid source activated local writer")
			}
		})
	}
}

func TestImportedResumeLostActivationAndUnknownStopKeepOneWriter(t *testing.T) {
	s, local, source := importedFixture(t)
	id := source.export.ID
	if _, err := s.Pull(t.Context(), id, source.export.OperationID, testRemote); err != nil {
		t.Fatal(err)
	}
	op := uuid.NewString()
	local.lostActivate = true
	if _, err := s.Resume(t.Context(), id, op); err == nil {
		t.Fatal("lost activation response was successful")
	}
	status, err := s.Resume(t.Context(), id, op)
	if err != nil || local.activations != 1 {
		t.Fatalf("lost activation repeated native launch: %+v %v", status, err)
	}
	local.unknownUsage = true
	if _, err := s.Stop(t.Context(), id, uuid.NewString()); err == nil {
		t.Fatal("missing local accounting became a committed checkpoint")
	}
	if _, err := s.Resume(t.Context(), id, uuid.NewString()); err == nil || local.activations != 1 {
		t.Fatal("unknown stopped source admitted another writer")
	}
	r, err := s.readCatalog(id)
	if err != nil || !reflect.DeepEqual(r.Committed.Receipt, source.export.Stopped.Receipt) {
		t.Fatalf("failed capture replaced prior revision: %+v %v", r, err)
	}
}

func TestStopMovedProductFinalizesWithoutDestinationAndRemainsReplayable(t *testing.T) {
	s, local, source := importedFixture(t)
	remote := s.placements[testRemote].(*placementFixture)
	id := source.export.ID
	if _, err := s.Pull(t.Context(), id, source.export.OperationID, testRemote); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resume(t.Context(), id, uuid.NewString()); err != nil {
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
	if _, err = s.Resume(t.Context(), id, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Move(t.Context(), id, moveOperation, testRemote); err == nil {
		t.Fatal("old move operation acquired a later execution")
	}
}

func TestIdleStopReplayCannotStopTheFirstContinuation(t *testing.T) {
	s, _, source := importedFixture(t)
	id := source.export.ID
	if _, err := s.Pull(t.Context(), id, source.export.OperationID, testRemote); err != nil {
		t.Fatal(err)
	}
	operation := uuid.NewString()
	if _, err := s.Stop(t.Context(), id, operation); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resume(t.Context(), id, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stop(t.Context(), id, operation); err == nil {
		t.Fatal("idle stop replay acquired a later writer")
	}
}

func TestPullRetentionRejectionDoesNotClaimOrActivateTheSource(t *testing.T) {
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
	s, local, source := importedFixture(t)
	id := source.export.ID
	if _, err := s.Pull(t.Context(), id, source.export.OperationID, testRemote); err != nil {
		t.Fatal(err)
	}
	s.config.Local.RetainedBytes = s.config.Local.MaxCheckpointBytes + 1
	if err := os.WriteFile(filepath.Join(s.initialDir, "retained-padding"), []byte("xx"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resume(t.Context(), id, uuid.NewString()); err == nil {
		t.Fatal("fresh execution bypassed retention bound")
	}
	if local.prepares != 0 || local.activations != 0 {
		t.Fatal("storage rejection admitted a writer")
	}
}
