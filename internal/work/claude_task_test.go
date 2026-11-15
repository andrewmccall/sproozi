package work

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/checkpoint"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/google/uuid"
)

const (
	claudeTaskThread = "3c1f0e52-7a4b-4d8e-9f61-2b5c8d7e9a01"
	claudeTaskResult = "Fixed the repository.\nTests pass."
)

// claudeTaskRow is one primary-chain transcript row in the shape the pinned
// client writes.
func claudeTaskRow(kind, id, parent, message string, content any) string {
	var parentID any
	if parent != "" {
		parentID = parent
	}
	body := map[string]any{"role": kind, "content": content}
	if message != "" {
		body["id"] = message
	}
	row, _ := json.Marshal(map[string]any{"type": kind, "sessionId": claudeTaskThread, "version": "2.1.318",
		"uuid": id, "parentUuid": parentID, "isSidechain": false, "cwd": "/workspace", "message": body})
	return string(row) + "\n"
}

func claudeTaskText(text string) []map[string]any {
	return []map[string]any{{"type": "text", "text": text}}
}

// claudeTaskCheckpoint is the stopped Claude checkpoint of a completed task
// turn: the launch's task is the one submitted prompt, and the turn's last
// reply follows an earlier one.
func claudeTaskCheckpoint(t *testing.T, repository, prompt string) ([]byte, checkpoint.Metadata, error) {
	path, err := harness.ClaudeProjectPath(repository, claudeTaskThread)
	if err != nil {
		return nil, checkpoint.Metadata{}, err
	}
	const user, first, second, third = "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222",
		"55555555-5555-4555-8555-555555555555", "66666666-6666-4666-8666-666666666666"
	transcript := claudeTaskRow("user", user, "", "", prompt) +
		claudeTaskRow("assistant", first, user, "msg_one", claudeTaskText("Let me look.")) +
		claudeTaskRow("assistant", second, first, "msg_two", claudeTaskText("Fixed the repository.")) +
		claudeTaskRow("assistant", third, second, "msg_two", claudeTaskText("Tests pass."))
	home := t.TempDir()
	state := harness.ClaudeState{ThreadID: claudeTaskThread, Workspace: repository, Path: path, Transcript: []byte(transcript)}
	if err = harness.RestoreClaudeState(home, repository, state, testLimit); err != nil {
		return nil, checkpoint.Metadata{}, err
	}
	return checkpoint.CaptureStopped(t.Context(), harness.NativeClaude, home, repository, claudeTaskThread, nil, testLimit)
}

// claudeExecutor is an owner for a Claude Code executor whose placement stops
// a task launch with the Claude checkpoint of the launch's own prompt, after
// reporting the task's turn as still running. The checkpoint's transcript
// carries the launch's prompt, so a result from it is that prompt's turn.
func claudeExecutor(t *testing.T) (*Owner, *fixturePlacement) {
	t.Helper()
	c, p := fixture(t)
	c.Harness = harness.NativeClaude
	p.turnPending = 2
	p.checkpointFor = func(l session.Launch) ([]byte, checkpoint.Metadata, string, error) {
		data, meta, err := claudeTaskCheckpoint(t, c.Repositories[repositorySproozi].Snapshot, l.Task)
		return data, meta, claudeTaskThread, err
	}
	return openOwner(t, c), p
}

// A Claude Code executor runs a task: the task is the launch's one prompt,
// the owner waits while that prompt's turn runs, and the retained result is
// Claude's final reply.
func TestClaudeExecutorRunsATaskToItsFinalReply(t *testing.T) {
	t.Parallel()
	o, p := claudeExecutor(t)
	q := Submit{RequestID: uuid.NewString(), Repository: repositorySproozi, Harness: harness.NativeClaude,
		Executor: executorKubernetes, Task: fixtureTask}
	v, err := o.Submit(t.Context(), q)
	if err != nil {
		t.Fatalf("Claude task refused: %v", err)
	}
	done := waitPhase(t, o, v.ID, phaseRetained)
	if done.Harness != harness.NativeClaude || done.ThreadID != claudeTaskThread || done.Result != claudeTaskResult ||
		done.ResultTruncated || done.Mode == ModeInteractive {
		t.Fatalf("retained Claude task: %+v", done)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.launches) != 1 {
		t.Fatalf("Claude task launched %d times", len(p.launches))
	}
	launched := p.launches[0]
	if launched.Harness != harness.NativeClaude || launched.ThreadID != "" || strings.Count(launched.Task, fixtureTask) != 1 {
		t.Fatalf("Claude task launched as %+v", launched)
	}
	if p.stops != 3 || p.turnPending != 0 {
		t.Fatalf("stopped before the task's turn completed: stops=%d pending=%d", p.stops, p.turnPending)
	}
	if _, err = o.Submit(t.Context(), interactiveRequest()); code(err) != profile.CodeUnsupported {
		t.Fatalf("Codex session accepted by a Claude executor: %v", err)
	}
}

// The work token, as Hermes holds it, submits a Claude task within the work
// token's own cap.
func TestWorkTokenSubmitsAClaudeTaskWithinItsCap(t *testing.T) {
	t.Parallel()
	o, _ := claudeExecutor(t)
	q := Submit{RequestID: uuid.NewString(), Repository: repositorySproozi, Harness: harness.NativeClaude,
		Executor: executorKubernetes, Task: fixtureTask, MaxDuration: "2m"}
	if _, err := o.SubmitAs(t.Context(), CallerWorkToken, q); code(err) != profile.CodeDenied {
		t.Fatalf("Claude task beyond the work token's cap: %v", err)
	}
	q.RequestID, q.MaxDuration = uuid.NewString(), ""
	before := time.Now()
	v, err := o.SubmitAs(t.Context(), CallerWorkToken, q)
	if err != nil {
		t.Fatalf("work token Claude task: %v", err)
	}
	if v.Deadline.Before(before.Add(time.Minute-time.Second)) || v.Deadline.After(time.Now().Add(time.Minute)) {
		t.Fatalf("work token deadline %v is not its one-minute cap", v.Deadline)
	}
	if done := waitPhase(t, o, v.ID, phaseRetained); done.Result != claudeTaskResult {
		t.Fatalf("work token Claude task: %+v", done)
	}
}
