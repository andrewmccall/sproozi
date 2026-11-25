package placement

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/harness"
)

// Runtime rules gate the building of a Pod (ADR 0077). A session whose Pod
// exists keeps working when its runtime is edited to break them: Activate
// enables it, Attach reaches and keeps its terminal, and Stop completes.

func TestActivateIgnoresARuntimeEditedAfterItsPod(t *testing.T) {
	t.Parallel()
	for name, fault := range launchRuntimeFaults {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, kube, _, l, data := setup(t)
			i, err := p.Prepare(t.Context(), l, data)
			if err != nil {
				t.Fatal(err)
			}
			editFixture(t, kube, fixtureRuntimeName, &api.AgentRuntime{}, fault.edit)
			if err = p.Activate(t.Context(), l, i); err != nil {
				t.Fatalf("Activate after the runtime was edited (%s): %v", fault.reason, err)
			}
		})
	}
}

// attachHolder holds a native attach open until its context ends or hold
// passes, and runs edit once the terminal is attached.
type attachHolder struct {
	*protocolExecutor
	hold time.Duration
	edit func()
}

func (a *attachHolder) Execute(ctx context.Context, namespace, pod, container string, args []string,
	input io.Reader, output, errors io.Writer,
) error {
	if len(args) < 2 || args[1] != string(harness.NativeAttach) {
		return a.protocolExecutor.Execute(ctx, namespace, pod, container, args, input, output, errors)
	}
	a.edit()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(a.hold):
		return nil
	}
}

func TestAttachKeepsItsTerminalWhenTheRuntimeIsEdited(t *testing.T) {
	t.Parallel()
	for name, fault := range launchRuntimeFaults {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p, kube, e, l, checkpointData := setup(t)
			l = freshLaunch(t, e, l, checkpointData, "")
			i, err := p.Prepare(t.Context(), l, e.initial)
			if err != nil {
				t.Fatal(err)
			}
			if err = p.Activate(t.Context(), l, i); err != nil {
				t.Fatal(err)
			}
			// The live check runs once a second; hold past two of them.
			p.exec = &attachHolder{protocolExecutor: e, hold: 2500 * time.Millisecond, edit: func() {
				editFixture(t, kube, fixtureRuntimeName, &api.AgentRuntime{}, fault.edit)
			}}
			if err = p.Attach(t.Context(), l, i, strings.NewReader(""), &bytes.Buffer{}); err != nil {
				t.Fatalf("attached terminal cut after the runtime was edited (%s): %v", fault.reason, err)
			}
		})
	}
}

func TestStopCompletesOnARuntimeEditedAfterItsPod(t *testing.T) {
	t.Parallel()
	p, kube, _, l, data := setup(t)
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	editFixture(t, kube, fixtureRuntimeName, &api.AgentRuntime{}, launchRuntimeFaults["managed shape"].edit)
	stopped, err := p.Stop(t.Context(), l, i)
	if err != nil || !stopped.ActualStopped || stopped.Receipt == nil {
		t.Fatalf("stop on an edited runtime: %+v %v", stopped, err)
	}
}

// A repeated Prepare of a launch whose registration exists (a lost
// acknowledgement, a recover) is not refused for a runtime edited since: the
// controller decides whether the Pod is still to be built.
func TestRepeatedPrepareIgnoresARuntimeEditedAfterRegistration(t *testing.T) {
	t.Parallel()
	p, kube, _, l, data := setup(t)
	i, err := p.Prepare(t.Context(), l, data)
	if err != nil {
		t.Fatal(err)
	}
	editFixture(t, kube, fixtureRuntimeName, &api.AgentRuntime{}, launchRuntimeFaults["managed shape"].edit)
	again, err := p.Prepare(t.Context(), l, data)
	if err != nil || again != i {
		t.Fatalf("repeated Prepare after the runtime was edited: %+v %v, want %+v", again, err, i)
	}
}
