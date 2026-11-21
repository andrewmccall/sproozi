package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// legacyHarnessArgs are harness arguments an earlier validator accepted and
// the current one refuses, as a journal written before the change holds them.
var legacyHarnessArgs = []string{`"-c","mcp_servers.mine.url=x"`, `"--model"`, `"-c=sandbox_mode=x"`}

// legacyExecution creates a first visit with harness arguments the current
// validator accepts, runs it when started is set, and then rewrites its
// journal to carry legacy instead, as one written before the tightening
// would. It returns the coordinator, the execution ID and the local
// placement.
func legacyExecution(t *testing.T, legacy string, started bool) (*Coordinator, string, *fakePlacement) {
	t.Helper()
	c, dir, local, _, s := setup(t)
	first := s.Source
	first.ExecutionID, first.OperationID, first.AllocationID = uuid.NewString(), uuid.NewString(), uuid.NewString()
	first.ThreadID, first.CheckpointDigest = "", ""
	first.HarnessArgs = []string{"--search"}
	if _, err := c.Create(first, []byte("captured git workspace"), ""); err != nil {
		t.Fatal(err)
	}
	if started {
		if _, err := c.Recover(t.Context(), first.ExecutionID); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, first.ExecutionID+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.ReplaceAll(string(data), `"--search"`, legacy)
	if edited == string(data) {
		t.Fatal("the journal holds no harness arguments to replace")
	}
	if err = os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	// The placement prepared the visit from the same earlier launch.
	var args []string
	if err = json.Unmarshal([]byte("["+legacy+"]"), &args); err != nil {
		t.Fatal(err)
	}
	for allocation, o := range local.records {
		o.Launch.HarnessArgs = args
		local.records[allocation] = o
	}
	return c, first.ExecutionID, local
}

// A journal whose harness arguments the current validator refuses still
// loads, and its session can still be stopped, cancelled and removed (ADR
// 0066: a record never blocks an end). Only starting or moving a visit with
// those arguments is refused, with the validator's message.
func TestRefusedHarnessArgumentsNeverStrandASession(t *testing.T) {
	keep := func() ([]string, error) { return nil, nil }
	admit := func() (func(), error) { return func() {}, nil }
	for _, legacy := range legacyHarnessArgs {
		t.Run(legacy, func(t *testing.T) {
			c, id, local := legacyExecution(t, legacy, true)
			if _, err := c.Inspect(id); err != nil {
				t.Fatalf("a running session with old arguments does not load: %v", err)
			}
			if _, err := c.Move(t.Context(), id, uuid.NewString(), testRemote); err == nil ||
				!strings.Contains(err.Error(), "harness argument") {
				t.Fatalf("moving a session with refused arguments: %v; want the validator's refusal", err)
			}
			local.turnless = true
			if _, err := c.Finalize(t.Context(), id, uuid.NewString()); err != nil {
				t.Fatalf("stopping a session with old arguments: %v", err)
			}
			if err := c.Remove([]string{id}, keep); err != nil {
				t.Fatalf("removing a stopped session with old arguments: %v", err)
			}

			c, id, local = legacyExecution(t, legacy, true)
			// The abort seals the visit's usage, so the cancelled session
			// settles.
			local.abortSealed = true
			if _, err := c.Cancel(t.Context(), id, uuid.NewString(), admit); err != nil {
				t.Fatalf("cancelling a running session with old arguments: %v", err)
			}
			if err := c.Remove([]string{id}, keep); err != nil {
				t.Fatalf("removing a cancelled session with old arguments: %v", err)
			}

			c, id, _ = legacyExecution(t, legacy, false)
			if _, err := c.Recover(t.Context(), id); err == nil || !strings.Contains(err.Error(), "harness argument") {
				t.Fatalf("starting a stored launch with refused arguments: %v; want the validator's refusal", err)
			}
			if _, err := c.Cancel(t.Context(), id, uuid.NewString(), admit); err != nil {
				t.Fatalf("cancelling a refused start: %v", err)
			}
			if err := c.Remove([]string{id}, keep); err != nil {
				t.Fatalf("removing a refused start: %v", err)
			}
		})
	}
}
