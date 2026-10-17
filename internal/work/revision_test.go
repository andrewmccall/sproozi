//nolint:goconst // Keep independent literal behavior expectations.
package work

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/workspace"
	"github.com/google/uuid"
)

func TestFreshNativeWorkStartsAtRecordedCommitAndKeepsSourceReceipt(t *testing.T) {
	c, p := projectFixture(t)
	o, err := Open(c)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	v, err := o.Submit(t.Context(), projectRequest("sproozi-dev"))
	if err != nil {
		t.Fatal(err)
	}
	source := waitPhase(t, o, v.ID, phaseRetained)
	if source.CheckpointInfo == nil || source.CheckpointInfo.Workspace.Revision == "" {
		t.Fatalf("Missing recorded Git revision: %#v", source)
	}
	q := projectRequest("sproozi-dev")
	q.WorkspaceFrom = &RevisionSource{SessionID: source.ID, Digest: source.CheckpointDigest,
		Revision: source.CheckpointInfo.Workspace.Revision}
	fresh, err := o.Submit(t.Context(), q)
	if err != nil || fresh.ID == source.ID || fresh.ExecutionID == source.ExecutionID {
		t.Fatalf("Fresh workspace reused old execution: %#v %v", fresh, err)
	}
	waitPhase(t, o, fresh.ID, phaseRetained)
	r, err := o.read(fresh.ID)
	if err != nil || r.Launch.ThreadID != "" || r.Launch.CheckpointDigest != "" {
		t.Fatalf("Workspace source became native resume: %#v %v", r.Launch, err)
	}
	data, err := os.ReadFile(o.path(fresh.ID, ".input"))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "checkout")
	if err = workspace.Restore(t.Context(), data, target, testLimit); err != nil {
		t.Fatal(err)
	}
	info, err := workspace.Inspect(data, testLimit)
	if err != nil || info.Revision != source.CheckpointInfo.Workspace.Revision {
		t.Fatalf("Fresh native input changed the recorded commit: %#v %v", info, err)
	}
	exported, err := o.BeginExport(t.Context(), source.ID, uuid.NewString())
	if err != nil || exported.Project != "sproozi-dev" || exported.CheckpointInfo.Workspace.Revision != info.Revision ||
		exported.Stopped.Receipt.Usage["model.inference"].Units != 7 {
		t.Fatalf("Source provenance/receipt changed: %#v %v", exported, err)
	}
	q.RequestID = uuid.NewString()
	q.WorkspaceFrom.Digest = "wrong-digest"
	if _, err = o.Submit(t.Context(), q); err == nil || OutcomeCode(err) != profile.CodeInvalid {
		t.Fatal("Unbound checkpoint selected a fresh execution")
	}
	q.WorkspaceFrom.Digest = source.CheckpointDigest
	q.WorkspaceFrom.Revision = "main"
	if _, err = o.Submit(t.Context(), q); err == nil || OutcomeCode(err) != profile.CodeInvalid {
		t.Fatal("A moving ref selected an exact recorded revision")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.initial != 2 {
		t.Fatal("Rejected source selectors prepared another writer")
	}
}
