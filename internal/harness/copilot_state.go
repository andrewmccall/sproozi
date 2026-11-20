package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
)

var copilotSessionID = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)

// CopilotState selects one Copilot conversation: the events journal of its
// session under session-state/. Copilot rebuilds the rest of the session
// directory, its workspace.yaml and its session-store.db index from the
// journal on resume, so nothing else is carried. Configuration, sign-in and
// instructions come from the destination's launch (ADR 0048).
type CopilotState struct {
	ThreadID string
	Events   []byte
}

// CopilotEventsPath is the journal of a session, relative to COPILOT_HOME.
func CopilotEventsPath(threadID string) (string, error) {
	if !copilotSessionID.MatchString(threadID) {
		return "", errors.New("invalid Copilot session selection")
	}
	return "session-state/" + threadID + "/events.jsonl", nil
}

// CaptureCopilotState reads the session's journal after the caller has
// stopped every native writer, and checks that its last turn completed. A
// final row the stopped writer did not finish is left out (stoppedJournal).
func CaptureCopilotState(home, threadID string, maxBytes int64) (CopilotState, error) {
	data, err := readCopilotEvents(home, threadID, maxBytes)
	if err != nil {
		return CopilotState{}, err
	}
	state := CopilotState{ThreadID: threadID, Events: stoppedJournal(data)}
	if _, err = CompletedCopilotTurn(state, maxBytes); err != nil {
		return CopilotState{}, refusedState(err)
	}
	return state, nil
}

// captureCopilot is Copilot's Harness.CaptureState; the journal's path does
// not depend on the workspace.
func captureCopilot(home, _, threadID string, maxBytes int64) (ConversationState, error) {
	return capturedState(CaptureCopilotState(home, threadID, maxBytes))
}

// Kind is NativeCopilot.
func (s CopilotState) Kind() NativeKind { return NativeCopilot }

// Thread is the Copilot session ID.
func (s CopilotState) Thread() string { return s.ThreadID }

// Size is the journal's length.
func (s CopilotState) Size() int { return len(s.Events) }

// CompletedTurn is CompletedCopilotTurn.
func (s CopilotState) CompletedTurn(maxBytes int64) (string, error) {
	return CompletedCopilotTurn(s, maxBytes)
}

// Restore is RestoreCopilotState; the journal's path does not depend on the
// workspace.
func (s CopilotState) Restore(home, _ string, maxBytes int64) error {
	return RestoreCopilotState(home, s, maxBytes)
}

// ValidateContinuity finds no evidence to compare: Copilot keeps no approved
// instructions in its journal, and the supervisor writes
// copilot-instructions.md afresh at each launch.
func (s CopilotState) ValidateContinuity(string, int64) error { return nil }

// FinalText is the main agent's last answer that requested no tools.
func (s CopilotState) FinalText(maxBytes int64) (string, error) {
	if _, err := CompletedCopilotTurn(s, maxBytes); err != nil {
		return "", err
	}
	var text string
	for line := range bytes.SplitSeq(s.Events, []byte{'\n'}) {
		var row struct {
			Type string `json:"type"`
			Data struct {
				Content      string            `json:"content"`
				ToolRequests []json.RawMessage `json:"toolRequests"`
				Parent       string            `json:"parentToolCallId"`
			} `json:"data"`
		}
		if json.Unmarshal(line, &row) == nil && row.Type == "assistant.message" && row.Data.Parent == "" &&
			len(row.Data.ToolRequests) == 0 {
			text = row.Data.Content
		}
	}
	return text, nil
}

// ReadCopilotState reads a live session's journal for observation, without
// requiring a completed turn.
func ReadCopilotState(home, threadID string, maxBytes int64) (CopilotState, error) {
	data, err := readCopilotEvents(home, threadID, maxBytes)
	return CopilotState{ThreadID: threadID, Events: data}, err
}

func readCopilotEvents(home, threadID string, maxBytes int64) ([]byte, error) {
	relative, err := CopilotEventsPath(threadID)
	if err != nil || maxBytes <= 0 {
		return nil, errors.New("invalid Copilot checkpoint selection")
	}
	if err = stateDirectories(home, filepath.Dir(relative), false, copilotUnsafeDirectory); err != nil {
		return nil, err
	}
	name := filepath.Join(home, filepath.FromSlash(relative))
	info, err := os.Lstat(name)
	if err != nil {
		return nil, stateFileError(err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxBytes {
		return nil, refusedState(errors.New("unsafe or oversized Copilot session journal"))
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, stateFileError(err)
	}
	data, readErr := io.ReadAll(io.LimitReader(f, maxBytes+1))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil {
		return nil, stateFileError(errors.Join(readErr, closeErr))
	}
	if int64(len(data)) > maxBytes {
		return nil, refusedState(errors.New("unsafe or oversized Copilot session journal"))
	}
	return data, nil
}

// RestoreCopilotState publishes the same journal bytes at the destination's
// session path. An existing journal is never overwritten.
func RestoreCopilotState(home string, state CopilotState, maxBytes int64) error {
	if _, err := CompletedCopilotTurn(state, maxBytes); err != nil {
		return err
	}
	relative, err := CopilotEventsPath(state.ThreadID)
	if err != nil {
		return err
	}
	if err = stateDirectories(home, filepath.Dir(relative), true, copilotUnsafeDirectory); err != nil {
		return err
	}
	return writeStateFile(filepath.Join(home, filepath.FromSlash(relative)), state.Events)
}

// copilotUnsafeDirectory refuses a link or non-directory on the journal's
// path.
const copilotUnsafeDirectory = "unsafe Copilot native directory"

type copilotEvent struct {
	ID       string          `json:"id"`
	ParentID *string         `json:"parentId"`
	Type     string          `json:"type"`
	Data     json.RawMessage `json:"data"`
}

type copilotData struct {
	SessionID        string            `json:"sessionId"`
	ParentToolCallID string            `json:"parentToolCallId"`
	ToolCallID       string            `json:"toolCallId"`
	ToolRequests     []json.RawMessage `json:"toolRequests"`
}

// ErrCopilotTurnIncomplete reports a journal whose last turn has not
// completed: no prompt yet, a model turn or tool still running, or a turn
// that ended on an error or an abort.
var ErrCopilotTurnIncomplete = errors.New("copilot session has no completed turn")

// CompletedCopilotTurn returns the ID of the journal's last completed turn:
// the main agent's final assistant.turn_end after its last prompt, following
// an answer that requested no tools, with every tool execution finished and
// no error or abort after it. Each event must name an earlier event as its
// parent, and the journal must start this session. Other event types stay
// opaque history.
func CompletedCopilotTurn(state CopilotState, maxBytes int64) (string, error) {
	if !copilotSessionID.MatchString(state.ThreadID) || maxBytes <= 0 || len(state.Events) == 0 ||
		int64(len(state.Events)) > maxBytes || !bytes.HasSuffix(state.Events, []byte{'\n'}) {
		return "", errors.New("invalid or oversized Copilot checkpoint")
	}
	o := copilotObserver{seen: map[string]bool{}, tools: map[string]bool{}}
	for line := range bytes.SplitSeq(state.Events, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		if err := o.accept(line, state.ThreadID); err != nil {
			return "", err
		}
	}
	if !o.prompted || o.open || len(o.tools) != 0 || !o.answered || o.completed == "" {
		return "", ErrCopilotTurnIncomplete
	}
	return o.completed, nil
}

// CopilotPrompted reports that the journal records at least one prompt.
func CopilotPrompted(state CopilotState) bool {
	return bytes.Contains(state.Events, []byte(`"type":"user.message"`))
}

// CopilotTurnless reports a journal with no conversation to keep: no prompt
// yet, or every prompt ended on an error or abort before any turn completed.
// A failed journal is not a resumable checkpoint. A journal that doesn't
// parse is not turnless, so a stop keeps waiting for it.
func CopilotTurnless(state CopilotState, maxBytes int64) bool {
	if int64(len(state.Events)) > maxBytes || len(state.Events) != 0 && !bytes.HasSuffix(state.Events, []byte{'\n'}) {
		return false
	}
	o := copilotObserver{seen: map[string]bool{}, tools: map[string]bool{}}
	for line := range bytes.SplitSeq(state.Events, []byte{'\n'}) {
		if len(line) != 0 && o.accept(line, state.ThreadID) != nil {
			return false
		}
	}
	return !o.prompted || !o.everCompleted && !o.open && len(o.tools) == 0 && o.failed
}

type copilotObserver struct {
	seen  map[string]bool
	tools map[string]bool
	// prompted: a prompt arrived; open: a main turn started and has not
	// ended; answered: the main agent's last message requested no tools;
	// completed: the turn_end event that closed that answer.
	started, prompted, open, answered bool
	completed                         string
	// everCompleted: some turn completed; failed: the last prompt's turn
	// ended on an error or abort.
	everCompleted, failed bool
}

func (o *copilotObserver) accept(line []byte, threadID string) error {
	var e copilotEvent
	if err := json.Unmarshal(line, &e); err != nil || e.ID == "" || e.Type == "" || o.seen[e.ID] {
		return errors.New("invalid Copilot session event")
	}
	if !o.started {
		if e.ParentID != nil || e.Type != "session.start" {
			return errors.New("copilot journal does not start its session")
		}
	} else if e.ParentID == nil || !o.seen[*e.ParentID] {
		return errors.New("copilot session event has no earlier parent")
	}
	o.seen[e.ID] = true
	var d copilotData
	if len(e.Data) != 0 {
		if err := json.Unmarshal(e.Data, &d); err != nil {
			return errors.New("invalid Copilot session event data")
		}
	}
	main := d.ParentToolCallID == ""
	switch e.Type {
	case "session.start":
		if o.started || d.SessionID != threadID {
			return errors.New("copilot journal belongs to another session")
		}
		o.started = true
	case "user.message":
		if main {
			o.prompted, o.answered, o.completed, o.failed = true, false, "", false
		}
	case "assistant.turn_start":
		if main {
			o.open, o.completed = true, ""
		}
	case "assistant.message":
		if main {
			o.answered = len(d.ToolRequests) == 0
		}
	case "assistant.turn_end":
		if main {
			o.open = false
			if o.answered {
				o.completed, o.everCompleted = e.ID, true
			}
		}
	case "tool.execution_start":
		if d.ToolCallID == "" {
			return errors.New("copilot tool execution has no call ID")
		}
		o.tools[d.ToolCallID] = true
	case "tool.execution_complete":
		if !o.tools[d.ToolCallID] {
			return errors.New("unmatched Copilot tool completion")
		}
		delete(o.tools, d.ToolCallID)
	case "session.error", "abort":
		o.answered, o.completed, o.failed = false, "", true
	}
	return nil
}
