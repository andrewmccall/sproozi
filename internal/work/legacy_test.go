package work

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/google/uuid"
)

// writeLegacy writes a version 1 record in phase, with the stopped evidence
// and checkpoint of export, as the previous release kept them.
func writeLegacy(t *testing.T, c Config, o *Owner, phase legacyPhase, export Export) string {
	t.Helper()
	request := uuid.NewString()
	id := o.sessionID(request)
	intent := Submit{RequestID: request, Repository: repositorySproozi, Harness: harness.NativeCodex,
		Executor: executorKubernetes, Task: fixtureTask}
	b, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	r := legacyRecord{Version: legacyVersion, Intent: intent, IntentDigest: hash(b), Phase: phase,
		Launch: session.Launch{Harness: harness.NativeCodex, ExecutionID: id, OperationID: request,
			AllocationID: uuid.NewString(), Epoch: 1, Placement: executorKubernetes,
			InstructionDigest: c.InstructionDigest, Allowances: c.Allowances, Task: fixtureTask},
		Stopped: &export.Stopped, CheckpointInfo: export.CheckpointInfo, Result: testResult}
	r.Association.Repository = repositorySproozi
	if b, err = json.Marshal(r); err != nil {
		t.Fatal(err)
	}
	if err = privatefs.Replace(c.Directory, id+".json", b); err != nil {
		t.Fatal(err)
	}
	if err = privatefs.Replace(c.Directory, id+".checkpoint", export.Checkpoint); err != nil {
		t.Fatal(err)
	}
	return id
}

// A stopped version 1 record still lists, exports and claims after the
// upgrade; a live one stops the executor from starting, naming it.
func TestVersionOneRecordsAreReadOnly(t *testing.T) {
	c, p := fixture(t)
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	v := submit(t, o)
	waitPhase(t, o, v.ID, phaseRetained)
	export, err := o.BeginExport(t.Context(), v.ID, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	retained := writeLegacy(t, c, o, legacyRetained, export)
	if err = o.Close(); err != nil {
		t.Fatal(err)
	}
	if o, err = Open(c); err != nil {
		t.Fatal(err)
	}
	listed, err := o.List(t.Context(), SessionQuery{})
	if err != nil || len(listed) != 2 {
		t.Fatalf("catalogue with a version 1 record: %+v %v", listed, err)
	}
	old, err := o.Info(t.Context(), retained)
	if err != nil || old.Phase != PhaseRetained || old.Result != testResult || old.CheckpointDigest != p.meta.SHA256 {
		t.Fatalf("version 1 retained record: %+v %v", old, err)
	}
	if _, err = o.Cancel(t.Context(), retained, uuid.NewString()); err == nil {
		t.Fatal("cancelled a retained version 1 record")
	}
	op := uuid.NewString()
	if _, err = o.BeginExport(t.Context(), retained, op); err != nil {
		t.Fatal(err)
	}
	if claim, err := o.Claim(t.Context(), retained, op, p.meta.SHA256); err != nil || !claim.Claimed {
		t.Fatalf("claim a version 1 record: %+v %v", claim, err)
	}
	if pulled, err := o.Info(t.Context(), retained); err != nil || pulled.Phase != PhasePulled {
		t.Fatalf("claimed version 1 record: %+v %v", pulled, err)
	}
	live := writeLegacy(t, c, o, "unknown", export)
	if err = o.Close(); err != nil {
		t.Fatal(err)
	}
	if o, err = Open(c); err == nil {
		_ = o.Close()
		t.Fatal("started with a live version 1 record")
	} else if !strings.Contains(err.Error(), live) || !strings.Contains(err.Error(), "previous release") {
		t.Fatalf("refusal does not name the record and the remedy: %v", err)
	}
}
