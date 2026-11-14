package placement

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/harness"
	runtimekube "github.com/andrewmccall/sproozi/internal/runtime/kubernetes"
	"github.com/andrewmccall/sproozi/internal/runtime/kubernetes/registry"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/workspace"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
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
	t.Parallel()
	p, kube, e, l, checkpointData := setup(t)
	l.ThreadID, l.CheckpointDigest, l.Task = "", "", initialFixtureTask
	e.initial = initialWorkspace(t, checkpointData)
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
	if stopped.ThreadID != fixtureThread || stopped.Launch.Task != initialFixtureTask || !stopped.ActualStopped || stopped.Receipt == nil || string(stopped.Checkpoint) != string(checkpointData) {
		t.Fatalf("incomplete initial result: %+v", stopped)
	}
	if !session.SameLaunch(stopped.Launch, l) || !session.SameLaunch(stopped.Receipt.Launch, l) {
		t.Fatalf("binding rewrote the immutable launch: %+v", stopped.Launch)
	}
	retry, err = p.CreateInitial(t.Context(), l, e.initial)
	if err != nil || retry != i {
		t.Fatal("stopped creation retry", retry, err)
	}
	repeated, err := p.Stop(t.Context(), l, i)
	if err != nil || repeated.CheckpointDigest != stopped.CheckpointDigest || repeated.ThreadID != fixtureThread || e.finishes != 1 {
		t.Fatal("original launch replay failed", repeated, err)
	}
	rewritten := l
	rewritten.ThreadID = fixtureThread
	if _, err = p.Inspect(t.Context(), rewritten); err == nil {
		t.Fatal("a launch rewritten with the bound thread was accepted")
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
	t.Parallel()
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
	t.Parallel()
	for name, create := range map[string]func(*Placement) func(context.Context, session.Launch, []byte) (session.Incarnation, error){
		"Prepare": func(p *Placement) func(context.Context, session.Launch, []byte) (session.Incarnation, error) {
			return p.Prepare
		},
		"CreateInitial": func(p *Placement) func(context.Context, session.Launch, []byte) (session.Incarnation, error) {
			return p.CreateInitial
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, kube, _, l, data := setupKind(t, harness.NativeClaude)
			l.ThreadID, l.CheckpointDigest, l.Task = "", "", "Task"
			_, err := create(p)(context.Background(), l, initialWorkspace(t, data))
			var rejection *profile.Rejection
			if !errors.As(err, &rejection) || rejection.Code != profile.CodeDenied || kube.runCreates != 0 {
				t.Fatalf("unproven task accepted: %v", err)
			}
		})
	}
}

// freshLaunch is a first visit from a Git workspace: no thread and no
// checkpoint, as the coordinator starts every first visit.
func freshLaunch(t *testing.T, e *protocolExecutor, l session.Launch, checkpointData []byte, task string) session.Launch {
	t.Helper()
	l.ThreadID, l.CheckpointDigest, l.Task = "", "", task
	e.initial = initialWorkspace(t, checkpointData)
	return l
}

func TestPrepareOfFreshLaunchMatchesCreateInitial(t *testing.T) {
	t.Parallel()
	viaPrepare, prepareKube, prepareExec, l, data := setup(t)
	l = freshLaunch(t, prepareExec, l, data, initialFixtureTask)
	viaCreate, createKube, createExec, _, _ := setup(t)
	createExec.initial = prepareExec.initial

	i, err := viaPrepare.Prepare(t.Context(), l, prepareExec.initial)
	if err != nil {
		t.Fatal(err)
	}
	created, err := viaCreate.CreateInitial(t.Context(), l, createExec.initial)
	if err != nil {
		t.Fatal(err)
	}
	_, prepared, err := viaPrepare.retained(t.Context(), l)
	if err != nil {
		t.Fatal(err)
	}
	_, initial, err := viaCreate.retained(t.Context(), l)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(prepared.Registration, initial.Registration) || prepared.WorkspaceDigest == "" || prepared.Phase != initial.Phase || prepared.Phase != registry.Prepared {
		t.Fatalf("Prepare registered %+v, CreateInitial %+v", prepared, initial)
	}
	if !validIncarnation(i) || !validIncarnation(created) {
		t.Fatal("invalid incarnation", i, created)
	}

	// Either entry replays the same incarnation without a second Run.
	for _, replay := range []func(context.Context, session.Launch, []byte) (session.Incarnation, error){viaPrepare.Prepare, viaPrepare.CreateInitial} {
		again, err := replay(t.Context(), l, prepareExec.initial)
		if err != nil || again != i {
			t.Fatal("fresh preparation replay", again, err)
		}
	}
	if prepareKube.runCreates != 1 || createKube.runCreates != 1 {
		t.Fatalf("Runs created: Prepare %d, CreateInitial %d", prepareKube.runCreates, createKube.runCreates)
	}

	// A different workspace for the same launch conflicts through either entry.
	for _, conflicting := range []func(context.Context, session.Launch, []byte) (session.Incarnation, error){viaPrepare.Prepare, viaPrepare.CreateInitial} {
		_, err = conflicting(t.Context(), l, append(append([]byte(nil), prepareExec.initial...), ' '))
		var rejection *profile.Rejection
		if !errors.As(err, &rejection) || rejection.Code != profile.CodeConflict {
			t.Fatalf("changed initial workspace: %v", err)
		}
	}
}

func validIncarnation(i session.Incarnation) bool {
	return idValid(i.ExecutionID) && i.ResourceUID != ""
}

func TestTurnlessFirstVisitStopsWithoutCheckpoint(t *testing.T) {
	t.Parallel()
	for _, kind := range []harness.NativeKind{harness.NativeCodex, harness.NativeClaude} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			p, kube, e, l, data := setupKind(t, kind)
			l = freshLaunch(t, e, l, data, "")
			i, err := p.Prepare(t.Context(), l, e.initial)
			if err != nil {
				t.Fatal(err)
			}
			if err = p.Activate(t.Context(), l, i); err != nil {
				t.Fatal(err)
			}
			e.turnless = true
			stopped, err := p.Stop(t.Context(), l, i)
			if err != nil {
				t.Fatal(err)
			}
			if stopped.Receipt == nil {
				t.Fatalf("turnless stop has no sealed receipt: %+v", stopped)
			}
			usage, ok := stopped.Receipt.Usage[string(api.CapabilityModelInference)]
			if !stopped.ActualStopped || !ok || stopped.ThreadID != "" || stopped.Checkpoint != nil || stopped.CheckpointDigest != "" || stopped.Incarnation != i {
				t.Fatalf("turnless stop: %+v", stopped)
			}
			if usage.Units != 0 || e.captureArgs != nil || e.finishes != 1 {
				t.Fatalf("turnless stop usage %+v, capture %v, finishes %d", usage, e.captureArgs, e.finishes)
			}
			var pod corev1.Pod
			if err = kube.Get(t.Context(), client.ObjectKey{Namespace: runtimekube.AgentsNamespace, Name: runtimekube.RunName(types.UID(i.ExecutionID))}, &pod); err != nil {
				t.Fatal(err)
			}
			if slices.Contains(pod.Finalizers, runtimekube.ManagedWriterFenceFinalizer) {
				t.Fatal("writer fence kept after the turnless stop")
			}
			repeated, err := p.Stop(t.Context(), l, i)
			if err != nil || !reflect.DeepEqual(repeated, stopped) {
				t.Fatalf("turnless stop replay: %+v %v", repeated, err)
			}
			observed, err := p.Inspect(t.Context(), l)
			if err != nil || observed.State != session.PlacementStopped || observed.Stopped == nil || !reflect.DeepEqual(*observed.Stopped, stopped) {
				t.Fatalf("turnless observation: %+v %v", observed, err)
			}
			aborted, err := p.Abort(t.Context(), l, i)
			if err != nil || aborted.State != session.PlacementStopped || !reflect.DeepEqual(*aborted.Stopped, stopped) {
				t.Fatalf("abort after turnless stop: %+v %v", aborted, err)
			}
		})
	}
}
