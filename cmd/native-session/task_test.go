package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/harness"
)

const (
	firstFixtureTurn = "turn-one"
	literalTask      = "--literal task"
	fixtureTask      = "Task"
	fixtureMCPTool   = "mcp__fixture__verify"
	fixtureToolID    = "toolu_x"
)

func TestCodexInitialTaskIsLiteralPositionalPrompt(t *testing.T) {
	for _, id := range []string{"", selected} {
		args := codexArguments(config{Task: literalTask, Instructions: instructions}, id)
		if !reflect.DeepEqual(args[len(args)-2:], []string{"--", literalTask}) {
			t.Fatalf("task was not a literal positional prompt: %q", args)
		}
		if id != "" && !reflect.DeepEqual(args[len(args)-4:len(args)-2], []string{"resume", selected}) {
			t.Fatalf("resume selection changed: %q", args)
		}
	}
}

func TestSubmittedTaskNeedsItsOwnCompletedNativeTurn(t *testing.T) {
	task := "Fix the fixture"
	prompt := `{"type":"response_item","payload":{"type":"message","role":"user","content":[` +
		`{"type":"input_text","text":"Fix the fixture"}]}}` + "\n"
	startNext := `{"type":"event_msg","payload":{"type":"task_started","turn_id":"turn-two"}}` + "\n"
	finishNext := `{"type":"event_msg","payload":{"type":"task_complete","turn_id":"turn-two"}}` + "\n"
	old := journal(prompt)
	for _, tc := range []struct {
		name, data string
		accept     bool
	}{
		{"fresh completed task", old, true},
		{"different prompt", journal(strings.ReplaceAll(prompt, task, "Another task")), false},
		{"new completed task", old + startNext + prompt + finishNext, true},
		{"old prompt followed by unrelated completed turn", old + startNext + finishNext, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := observeTaskTurn(writeJournal(t, tc.data), selected, digest(), 8192, task)
			if (err == nil) != tc.accept {
				t.Fatalf("accept=%v error=%v", tc.accept, err)
			}
		})
	}
}

func TestTaskContractRejectsUnsupportedAndUnboundedInput(t *testing.T) {
	c := config{Harness: harness.NativeCodex, Deadline: time.Now().Add(time.Minute),
		Instructions: instructions, InstructionDigest: digest(), ClientConfig: testClients,
		Env: []string{testPathEnvironment}, RolloutMaxBytes: 8192}
	for _, task := range []string{" ", "task\x00other", strings.Repeat("x", (32<<10)+1)} {
		c.Task = task
		if c.Validate(time.Now()) == nil {
			t.Fatal("invalid task accepted")
		}
	}
	c.Task, c.Harness = fixtureTask, harness.NativeClaude
	if err := c.Validate(time.Now()); err != nil {
		t.Fatalf("Claude task refused: %v", err)
	}
	c.Task, c.Harness = "", harness.NativeCopilot
	if err := c.Validate(time.Now()); err != nil {
		t.Fatalf("Copilot fixture launch refused without a task: %v", err)
	}
	c.Task = fixtureTask
	if c.Validate(time.Now()) == nil {
		t.Fatal("unproven Copilot task accepted")
	}
}

// The task is Claude's one positional prompt, after every option and the
// resume selection, behind "--" so a task that starts with a dash stays text.
func TestClaudeTaskIsOneLiteralPositionalPrompt(t *testing.T) {
	task := literalTask
	for _, thread := range []string{"", selected} {
		c := config{Harness: harness.NativeClaude, Task: task, ThreadID: thread, Instructions: instructions,
			ClientConfig: testClients, Args: []string{"--effort", "high"}}
		args := claudeArguments(c, agentprofile.Profile{}, false)
		if !reflect.DeepEqual(args[len(args)-2:], []string{"--", task}) {
			t.Fatalf("task was not the literal positional prompt: %q", args)
		}
		count := 0
		for _, arg := range args {
			if strings.Contains(arg, task) {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("task appears %d times in argv: %q", count, args)
		}
		if thread != "" && !reflect.DeepEqual(args[len(args)-4:len(args)-2], []string{"--resume", selected}) {
			t.Fatalf("resume selection changed: %q", args)
		}
	}
	args := claudeArguments(config{Harness: harness.NativeClaude, Instructions: instructions, ClientConfig: testClients},
		agentprofile.Profile{}, false)
	if slices.Contains(args, "--") {
		t.Fatalf("interactive launch has a prompt separator: %q", args)
	}
}

// claudeTaskServer is a fresh Claude task visit whose hooks name one native
// conversation.
func claudeTaskServer(task string) (*server, func(event, prompt string) response) {
	const id = "8a8f7c40-1d55-4f39-9e4e-6d2b7f3c9a10"
	s := &server{config: config{Harness: harness.NativeClaude, Task: task}, life: lifecycle{phase: phaseActive}}
	path, _ := harness.ClaudeProjectPath(workspace, id)
	inactive := false
	return s, func(event, prompt string) response {
		h := &claudeHook{SessionID: id, TranscriptPath: claudeHome + "/" + path, Cwd: workspace,
			Event: event, Prompt: prompt}
		if event == claudeStop {
			h.StopActive = &inactive
		}
		if event == claudePromptSubmit {
			// Each prompt is accepted terminal input.
			s.inputGeneration++
		}
		return s.claudeHook(h)
	}
}

func TestClaudeTaskCompletesOnlyAfterAStopThatFollowsTheTaskPrompt(t *testing.T) {
	task := "Fix the fixture\nthen report"
	s, hook := claudeTaskServer(task)
	for _, step := range []struct{ event, prompt string }{{claudePromptSubmit, "An earlier prompt"}, {claudeStop, ""}} {
		if r := hook(step.event, step.prompt); r.Error != "" {
			t.Fatal(r.Error)
		}
	}
	s.mu.Lock()
	ready := s.claudeTurnReady()
	s.mu.Unlock()
	if ready {
		t.Fatal("a Stop from an earlier prompt completed the task")
	}
	if d := s.claudeCompletion(); d.Observation != taskPromptUnobserved {
		t.Fatalf("task diagnostic: %+v", d)
	}
	if r := hook(claudePromptSubmit, task); r.Error != "" {
		t.Fatal(r.Error)
	}
	s.mu.Lock()
	ready = s.claudeTurnReady()
	s.mu.Unlock()
	if ready {
		t.Fatal("the task's running turn counted as complete")
	}
	if r := hook(claudeStop, ""); r.Error != "" {
		t.Fatal(r.Error)
	}
	s.mu.Lock()
	ready = s.claudeTurnReady()
	s.mu.Unlock()
	if !ready {
		t.Fatal("the task's completed turn was refused")
	}
	// A later prompt starts a turn that is not the task's.
	for _, step := range []struct{ event, prompt string }{{claudePromptSubmit, "Another prompt"}, {claudeStop, ""}} {
		if r := hook(step.event, step.prompt); r.Error != "" {
			t.Fatal(r.Error)
		}
	}
	s.mu.Lock()
	ready = s.claudeTurnReady()
	s.mu.Unlock()
	if ready {
		t.Fatal("a later prompt's Stop completed the task")
	}
}

// A task launch that resumes a conversation needs the task's own new turn:
// the restored conversation alone never completes it, and it is never
// turnless, even before any prompt.
func TestClaudeTaskLaunchIsNeverRestoredUnchangedOrTurnless(t *testing.T) {
	s := &server{config: config{Harness: harness.NativeClaude, Task: fixtureTask, ThreadID: selected},
		life: lifecycle{phase: phaseGated, gatedAt: time.Now().Add(-time.Hour)}}
	s.mu.Lock()
	restored := s.claudeRestoredUnchanged()
	s.mu.Unlock()
	if restored {
		t.Fatal("a restored conversation completed a task launch")
	}
	if _, _, err := s.observeClaudeTurn(); err == nil {
		t.Fatal("task launch observed a completed turn without one")
	}
	fresh := &server{config: config{Harness: harness.NativeClaude, Task: fixtureTask},
		life: lifecycle{phase: phaseGated, gatedAt: time.Now().Add(-time.Hour)}}
	if fresh.isTurnless() {
		t.Fatal("a fresh task launch was turnless")
	}
	if !fresh.claudeTurnless() {
		t.Fatal("fixture: the adapter alone should see no conversation")
	}
	fresh.config.Task = ""
	if !fresh.isTurnless() {
		t.Fatal("fixture: the same interactive visit should be turnless")
	}
}

// The pinned stock client (2.1.318) wrote these hook payloads for one task
// turn under the managed hooks: a foreground Bash call, then an MCP tool call,
// then Stop; the same with both calls failing; and a Skill call loading a
// profile skill and the sproozi-access skill. hack/verify/claude-transcripts
// captures them; paths are the managed ones.
func TestClaudeStockTaskHooksCompleteTheTaskTurn(t *testing.T) {
	for _, tc := range []struct{ file, task string }{
		{"claude-2.1.318-mcp-tool-hooks.jsonl", "--literal task: call the tool\n  then report.  "},
		{"claude-2.1.318-failed-tools-hooks.jsonl", "--literal task: call the tool"},
		{"claude-2.1.318-profile-skill-hooks.jsonl", "Use the fixture skill."},
		{"claude-2.1.318-sproozi-skill-hooks.jsonl", "Use the sproozi access skill."},
	} {
		t.Run(tc.file, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("testdata", tc.file))
			if err != nil {
				t.Fatal(err)
			}
			for _, task := range []string{tc.task, strings.TrimSpace(tc.task) + " (changed)"} {
				s := &server{config: config{Harness: harness.NativeClaude, Task: task}, life: lifecycle{phase: phaseActive}}
				for line := range strings.SplitSeq(strings.TrimSpace(string(data)), "\n") {
					h, err := readClaudeHook(strings.NewReader(line))
					if err != nil {
						t.Fatal(err)
					}
					if h.Event == claudePromptSubmit {
						s.inputGeneration++
					}
					if r := s.claudeHook(h); r.Error != "" {
						t.Fatalf("%s hook refused: %s", h.Event, r.Error)
					}
				}
				s.mu.Lock()
				ready, pending := s.claudeTurnReady(), len(s.claude.pending)
				s.mu.Unlock()
				if pending != 0 || ready != (task == tc.task) {
					t.Fatalf("task %q: ready=%v pending=%d", task, ready, pending)
				}
			}
		})
	}
}
