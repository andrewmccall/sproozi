package main

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
)

const (
	firstFixtureTurn = "turn-one"
)

func TestCodexInitialTaskIsLiteralPositionalPrompt(t *testing.T) {
	for _, id := range []string{"", selected} {
		args := codexArguments(config{Task: "--literal task", Instructions: instructions}, id)
		if !reflect.DeepEqual(args[len(args)-2:], []string{"--", "--literal task"}) {
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
		name, data, baseline string
		accept               bool
	}{
		{"fresh completed task", old, "", true},
		{"restored old completion", old, firstFixtureTurn, false},
		{"different prompt", journal(strings.ReplaceAll(prompt, task, "Another task")), "", false},
		{"new pending task", old + startNext + prompt, firstFixtureTurn, false},
		{"new completed task", old + startNext + prompt + finishNext, firstFixtureTurn, true},
		{"old prompt followed by unrelated completed turn", old + startNext + finishNext, firstFixtureTurn, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := observeTaskTurn(writeJournal(t, tc.data), selected, digest(), 8192, task, tc.baseline)
			if (err == nil) != tc.accept {
				t.Fatalf("accept=%v error=%v", tc.accept, err)
			}
		})
	}
}

func TestTaskContractRejectsUnsupportedAndUnboundedInput(t *testing.T) {
	c := config{Harness: harness.NativeCodex, Deadline: time.Now().Add(time.Minute),
		Instructions: instructions, InstructionDigest: digest(),
		Env: []string{testPathEnvironment}, RolloutMaxBytes: 8192}
	for _, task := range []string{" ", "task\x00other", strings.Repeat("x", (32<<10)+1)} {
		c.Task = task
		if c.Validate(time.Now()) == nil {
			t.Fatal("invalid task accepted")
		}
	}
	c.Task, c.Harness = "Task", harness.NativeClaude
	if c.Validate(time.Now()) == nil {
		t.Fatal("unproven Claude task accepted")
	}
}
