package work

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The outcomes of a full executor, spelled as on the wire so these tests
// state the protocol independently of the constants.
const (
	wireSessionsFull  = profile.Code("sessions_full")
	wireRetentionFull = profile.Code("retention_full")
)

// keptBytes is what the executor keeps on disk: its catalogue and its
// coordinator's directory.
func keptBytes(t *testing.T, c Config) int64 {
	t.Helper()
	var size int64
	for _, dir := range []string{c.Directory, filepath.Join(filepath.Dir(c.Directory), "sessions")} {
		err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			info, err := d.Info()
			if err == nil {
				size += info.Size()
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return size
}

func taskRequest() Submit {
	return Submit{RequestID: uuid.NewString(), Repository: repositorySproozi, Harness: harness.NativeCodex,
		Executor: executorKubernetes, Task: fixtureTask}
}

// A full executor says what is full and how to make room, over HTTP, MCP and
// in the CLI's message, instead of reporting itself unavailable.
func TestFullExecutorReportsWhatIsFull(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	c.RetainedBytes = 2 * c.MaxCheckpointBytes
	release := make(chan struct{})
	p.heldStop, p.stopStarted = release, make(chan struct{})
	o := openOwner(t, c)
	defer close(release)
	submit(t, o)

	_, err := o.Submit(t.Context(), taskRequest())
	if code(err) != wireRetentionFull {
		t.Fatalf("submission beyond retainedBytes: %v", err)
	}
	message := outcomeMessage(err)
	for _, want := range []string{"retainedBytes", `sproozi rm ID --remote`} {
		if !strings.Contains(message, want) {
			t.Errorf("retention refusal %q does not name %q", message, want)
		}
	}
	// Hermes reads the same text from sproozi_run.
	result, _, _ := tool(View{}, err)
	if result == nil || !result.IsError || result.Content[0].(*mcp.TextContent).Text != message {
		t.Fatalf("MCP refusal: %+v", result)
	}
	// The owner protocol carries the code with its own status.
	w := httptest.NewRecorder()
	writeOutcome(w, err)
	if w.Code != http.StatusInsufficientStorage || !strings.Contains(w.Body.String(), string(wireRetentionFull)) {
		t.Fatalf("HTTP refusal: %d %s", w.Code, w.Body.String())
	}

	// Fill the catalogue's count with placeholder records the count reads
	// by name alone.
	for range maxSessions {
		if err = os.WriteFile(filepath.Join(c.Directory, uuid.NewString()+".json"), []byte("{}"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	_, err = o.Submit(t.Context(), taskRequest())
	if code(err) != wireSessionsFull {
		t.Fatalf("submission beyond the session count: %v", err)
	}
	message = outcomeMessage(err)
	for _, want := range []string{"sessions", `sproozi rm ID --remote`} {
		if !strings.Contains(message, want) {
			t.Errorf("count refusal %q does not name %q", message, want)
		}
	}
}

// A submission reserves what its session may come to keep: its input, which
// the coordinator retains when it starts, and the checkpoint it commits when
// it stops.
func TestSubmissionReservesItsInputAndItsCheckpoint(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	o := openOwner(t, c)
	waitPhase(t, o, submit(t, o).ID, phaseRetained)
	kept := keptBytes(t, c)

	o.config.RetainedBytes = kept + c.MaxCheckpointBytes + c.MaxCheckpointBytes/2
	if _, err := o.Submit(t.Context(), taskRequest()); code(err) != wireRetentionFull {
		t.Fatalf("submission with room for its checkpoint but not its input: %v", err)
	}
	o.config.RetainedBytes = kept + 2*c.MaxCheckpointBytes
	if _, err := o.Submit(t.Context(), taskRequest()); err != nil {
		t.Fatalf("submission with room for its input and checkpoint: %v", err)
	}
}

// A resume makes the executor keep another checkpoint, so it is admitted as
// a submission is; its input is the session's own kept checkpoint.
func TestResumeRespectsRetainedBytes(t *testing.T) {
	t.Parallel()
	c, _ := fixture(t)
	o := openOwner(t, c)
	v := startInteractive(t, o, interactiveRequest())
	if _, err := o.Stop(t.Context(), v.ID, uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	kept := keptBytes(t, c)

	o.config.RetainedBytes = kept + c.MaxCheckpointBytes - 1
	if _, err := o.Resume(t.Context(), v.ID, uuid.NewString(), ""); code(err) != wireRetentionFull {
		t.Fatalf("resume beyond retainedBytes: %v", err)
	}
	if after, err := o.Info(t.Context(), v.ID); err != nil || after.ExecutionID != v.ExecutionID ||
		after.Phase != PhaseRetained {
		t.Fatalf("a refused resume changed the session: %+v %v", after, err)
	}
	o.config.RetainedBytes = kept + c.MaxCheckpointBytes
	resumed, err := o.Resume(t.Context(), v.ID, uuid.NewString(), "")
	if err != nil || resumed.ExecutionID == v.ExecutionID {
		t.Fatalf("resume within retainedBytes: %+v %v", resumed, err)
	}
	waitPhase(t, o, v.ID, phaseActive)
}

// Stopping is never refused for space: what a stop commits was reserved when
// its execution was admitted, and refusing it would lose the work.
func TestStopsCommitOnAFullExecutor(t *testing.T) {
	t.Parallel()
	c, p := fixture(t)
	o := openOwner(t, c)
	interactive := startInteractive(t, o, interactiveRequest())
	release := make(chan struct{})
	p.mu.Lock()
	p.heldStop, p.stopStarted = release, make(chan struct{})
	started := p.stopStarted
	p.mu.Unlock()
	task := submit(t, o)
	<-started

	o.config.RetainedBytes = 1
	if _, err := o.Submit(t.Context(), taskRequest()); code(err) != wireRetentionFull {
		t.Fatalf("submission to a full executor: %v", err)
	}
	close(release)
	waitPhase(t, o, task.ID, phaseRetained)
	p.mu.Lock()
	p.heldStop = nil
	p.mu.Unlock()
	if v, err := o.Stop(t.Context(), interactive.ID, uuid.NewString()); err != nil || v.Phase != PhaseRetained {
		t.Fatalf("interactive stop on a full executor: %+v %v", v, err)
	}
}
