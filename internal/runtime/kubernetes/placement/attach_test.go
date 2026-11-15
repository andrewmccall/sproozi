package placement

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session/profile"
)

var errAttachReached = errors.New("native attach reached")

// A remote executor attaches to the first visit of a session it started from a
// workspace. That visit has no conversation ID or checkpoint yet; attaching
// must check it as a first visit, as stop and inspect do, and reach the
// native terminal.
func TestAttachReachesTheTerminalOfAFreshFirstVisit(t *testing.T) {
	t.Parallel()
	for _, kind := range []harness.NativeKind{harness.NativeCodex, harness.NativeClaude} {
		t.Run(string(kind), func(t *testing.T) {
			t.Parallel()
			p, _, e, l, checkpointData := setupKind(t, kind)
			l = freshLaunch(t, e, l, checkpointData, "")
			i, err := p.Prepare(t.Context(), l, e.initial)
			if err != nil {
				t.Fatal(err)
			}
			if err = p.Activate(t.Context(), l, i); err != nil {
				t.Fatal(err)
			}
			var attached []string
			e.onCommand = func(command string) error {
				if command == string(harness.NativeAttach) {
					attached = append(attached, command)
					return errAttachReached
				}
				return nil
			}
			err = p.Attach(t.Context(), l, i, strings.NewReader(""), &bytes.Buffer{})
			if !errors.Is(err, errAttachReached) || len(attached) != 1 {
				t.Fatalf("attach to an active first visit: err = %v, attached %v; want the native terminal reached once", err, attached)
			}
		})
	}
}

// Attaching still refuses a visit that is not the active one.
func TestAttachRefusesAnotherIncarnation(t *testing.T) {
	t.Parallel()
	p, _, e, l, checkpointData := setup(t)
	l = freshLaunch(t, e, l, checkpointData, "")
	i, err := p.Prepare(t.Context(), l, e.initial)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Activate(t.Context(), l, i); err != nil {
		t.Fatal(err)
	}
	other := i
	other.ResourceUID = "00000000-0000-4000-8000-000000000000"
	var rejection *profile.Rejection
	err = p.Attach(t.Context(), l, other, strings.NewReader(""), &bytes.Buffer{})
	if !errors.As(err, &rejection) || rejection.Code != profile.CodeConflict {
		t.Fatalf("attach to another incarnation: err = %v, want conflict", err)
	}
}
