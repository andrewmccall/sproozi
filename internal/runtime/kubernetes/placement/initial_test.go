package placement

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/harness"
	registry "github.com/andrewmccall/sproozi/internal/runtime/kubernetes/session"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/workspace"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	initialFixtureTask = "Fix the fixture"
)

func initialWorkspace(t *testing.T, data []byte) []byte {
	t.Helper()
	pair := filepath.Join(t.TempDir(), "pair")
	if _, err := checkpoint.Restore(t.Context(), data, pair, fixtureMax); err != nil {
		t.Fatal(err)
	}
	artifact, err := workspace.Capture(t.Context(), filepath.Join(pair, "workspace"), nil, fixtureMax)
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}

func TestFreshRemoteWorkspaceStopsWithActualUUIDAndImmutableReceipt(t *testing.T) {
	p, kube, e, l, checkpointData := setup(t)
	l.ThreadID, l.CheckpointDigest, l.Task = "", "", initialFixtureTask
	e.initial = initialWorkspace(t, checkpointData)
	if _, err := p.Prepare(t.Context(), l, e.initial); err == nil {
		t.Fatal("resume accepted absent native identity")
	}
	i, err := p.CreateInitial(t.Context(), l, e.initial)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := p.CreateInitial(t.Context(), l, e.initial)
	if err != nil || retry != i {
		t.Fatal("prepared creation retry", retry, err)
	}
	observation, err := p.Inspect(t.Context(), l)
	if err != nil || observation.State != session.PlacementPrepared {
		t.Fatal("initial preparation", observation, err)
	}
	var run api.AgentRun
	if err = kube.Get(t.Context(), client.ObjectKey{Namespace: fixtureNamespace, Name: runName(l)}, &run); err != nil {
		t.Fatal(err)
	}
	if run.Spec.Task != initialFixtureTask {
		t.Fatalf("submitted task lost: %q", run.Spec.Task)
	}
	if err = p.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(t.Context(), l, i); err != nil {
		t.Fatal("active activation retry", err)
	}
	observation, err = p.Inspect(t.Context(), l)
	if err != nil || observation.State != session.PlacementActive {
		t.Fatal("initial active observation", observation, err)
	}
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil {
		t.Fatal(err)
	}
	if stopped.Launch.ThreadID != fixtureThread || stopped.Launch.Task != initialFixtureTask || !stopped.ActualStopped || stopped.Receipt == nil || string(stopped.Checkpoint) != string(checkpointData) {
		t.Fatalf("incomplete initial result: %+v", stopped)
	}
	retry, err = p.CreateInitial(t.Context(), l, e.initial)
	if err != nil || retry != i {
		t.Fatal("stopped creation retry", retry, err)
	}
	repeated, err := p.Stop(t.Context(), l, i)
	if err != nil || repeated.CheckpointDigest != stopped.CheckpointDigest || repeated.Receipt.Launch.ThreadID != fixtureThread || e.finishes != 1 {
		t.Fatal("original launch replay failed", repeated, err)
	}
	if _, err = p.Inspect(t.Context(), stopped.Launch); err != nil {
		t.Fatal("bound launch replay failed", err)
	}
	changed := l
	changed.Task = "A different task"
	if _, err = p.Inspect(t.Context(), changed); err == nil {
		t.Fatal("changed initial task accepted")
	}
	if _, err = p.CreateInitial(t.Context(), l, append(e.initial, ' ')); err == nil {
		t.Fatal("changed initial workspace accepted")
	}
}

func TestInitialUnknownTurnCannotSealOrCapture(t *testing.T) {
	p, _, e, l, data := setup(t)
	l.ThreadID, l.CheckpointDigest, l.Task = "", "", initialFixtureTask
	e.initial = initialWorkspace(t, data)
	i, err := p.CreateInitial(t.Context(), l, e.initial)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	e.failQuiesce = true
	if _, err = p.Stop(t.Context(), l, i); err == nil {
		t.Fatal("unknown turn accepted")
	}
	local, record, err := p.retained(t.Context(), l)
	if err != nil {
		t.Fatal(err)
	}
	if local.OutputDigest != "" || local.WriterFenced || record.Phase != registry.Active || record.Receipt != nil || e.finishes != 0 {
		t.Fatalf("unknown turn released execution: %+v %+v", local, record)
	}
}

func TestInitialTaskRejectsUnsupportedHarness(t *testing.T) {
	p, _, _, l, data := setupKind(t, harness.NativeClaude)
	l.ThreadID, l.CheckpointDigest, l.Task = "", "", "Task"
	_, err := p.CreateInitial(context.Background(), l, initialWorkspace(t, data))
	var rejection *profile.Rejection
	if !errors.As(err, &rejection) || rejection.Code != profile.CodeDenied {
		t.Fatalf("unproven task accepted: %v", err)
	}
}
