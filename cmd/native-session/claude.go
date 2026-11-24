package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/harness"
)

const (
	journalUnavailable = "journal_unavailable_or_invalid"
	pendingTools       = "pending_tools"
	inputChanged       = "input_changed"
	claudePromptSubmit = "UserPromptSubmit"
	claudePreTool      = "PreToolUse"
	claudePostTool     = "PostToolUse"
	// claudePostToolFailure ends a tool call that failed: a command that
	// exited non-zero or an MCP tool that reported an error.
	claudePostToolFailure = "PostToolUseFailure"
	claudeStop            = "Stop"
	claudeStopFailure     = "StopFailure"
	claudeBash            = harness.ClaudeBash
	claudeHooksKey        = "hooks"

	claudeSettings = "/tmp/sproozi-native/claude-settings.json"
	// claudeHookCommand is what each lifecycle hook runs.
	claudeHookCommand = "/usr/local/bin/native-session observe-claude"
)

const opClaudeHook harness.NativeOp = "observe-claude"

// Native hooks and their journal are workload-accessible completion hints. They
// authorize only controlled process shutdown, never accounting or writer fences.
type claudeHook struct {
	SessionID      string `json:"session_id"`
	TranscriptPath string `json:"transcript_path"`
	Cwd            string `json:"cwd"`
	Event          string `json:"hook_event_name"`
	ToolID         string `json:"tool_use_id"`
	ToolName       string `json:"tool_name"`
	StopActive     *bool  `json:"stop_hook_active"`
	// Interrupt is PostToolUseFailure's report that the user interrupted
	// the call rather than the call failing.
	Interrupt *bool `json:"is_interrupt"`
	// Prompt is UserPromptSubmit's submitted text. A task launch compares it
	// with its task and keeps only the result.
	Prompt string `json:"prompt"`
}

// claudeTurn is where the current Claude turn stands. accept is its only
// transition function.
type claudeTurn int

const (
	// turnIdle: no prompt was submitted in this visit.
	turnIdle claudeTurn = iota
	// turnRunning: a prompt was submitted and the turn has not ended; only
	// a running turn starts tools.
	turnRunning
	// turnCompleted: the turn ended with a safe Stop.
	turnCompleted
	// turnFailed: the turn ended on an API error (StopFailure). Only a new
	// prompt starts another turn.
	turnFailed
)

type claudeLifecycle struct {
	id   string
	turn claudeTurn
	// everCompleted records that some turn of this conversation completed.
	everCompleted bool
	// lastHook is when the last lifecycle hook arrived.
	lastHook time.Time
	input    uint64
	// pending maps each running tool's ID to its name.
	pending   map[string]string
	lastEvent string
	rejected  string
	// taskTurn records that the current turn's prompt was the launch's task,
	// the counterpart of Codex's task text in its new completed turn.
	taskTurn bool
}

// taskPromptUnobserved: a task launch's completed turn did not follow its
// task's prompt.
const taskPromptUnobserved = "task_prompt_unobserved"

// Completion diagnostics describe the observer's state without disclosing
// prompts, tool arguments, native journal bytes or selected filesystem paths.
type claudeCompletion = harness.NativeCompletion

func (s *server) claudeCompletion() *claudeCompletion {
	s.mu.Lock()
	c := s.claude
	d := &claudeCompletion{Started: c.turn != turnIdle, Completed: c.turn == turnCompleted, PendingTools: len(c.pending),
		PromptGeneration: c.input, InputGeneration: s.inputGeneration,
		LastEvent: c.lastEvent, Rejected: c.rejected, Observation: "lifecycle_incomplete"}
	selected := s.config.ThreadID
	settled := s.inputSettled()
	restored := s.claudeRestoredUnchanged()
	untasked := s.config.Task != "" && !c.taskTurn
	s.mu.Unlock()
	switch {
	case d.PendingTools != 0:
		d.Observation = pendingTools
	case d.PromptGeneration != d.InputGeneration && !settled:
		d.Observation = inputChanged
	case !restored && (c.id == "" || (selected != "" && c.id != selected)):
		d.Observation = "identity_unavailable"
	case d.Completed && untasked:
		d.Observation = taskPromptUnobserved
	case d.Completed || restored:
		if _, _, err := s.observeClaudeTurn(); err != nil {
			d.Observation = claudeJournalDiagnostic(err)
		} else {
			d.Observation = "settled"
		}
	}
	return d
}

func claudeJournalDiagnostic(err error) string {
	// Match only fixed adapter errors. Unknown content-block names and OS path
	// errors must not cross the diagnostic boundary as native payloads.
	switch err.Error() {
	case "unsupported Claude stop hook summary":
		return "journal_stop_summary_invalid"
	case "incompatible Claude primary transcript chain":
		return "journal_primary_chain_invalid"
	case "unsupported Claude progress record", "unmatched Claude stop hook progress",
		"unmatched Claude pre-tool hook progress", "unmatched Claude post-tool hook progress":
		return "journal_hook_progress_invalid"
	case "claude transcript has no settled final assistant leaf":
		return "journal_leaf_unsettled"
	default:
		return journalUnavailable
	}
}

func (s *server) claudeHook(h *claudeHook) response {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := response{Op: opClaudeHook, State: s.life.phase.wire()}
	if h == nil || s.config.Harness != harness.NativeClaude || !s.life.phase.running() {
		r.Error = "Claude lifecycle observer is unavailable"
		s.claude.rejected = "observer_unavailable"
		return r
	}
	relative, err := harness.ClaudeProjectPath(workspace, h.SessionID)
	if err != nil || h.Cwd != workspace || h.TranscriptPath != filepath.Join(claudeHome, relative) ||
		(s.config.ThreadID != "" && h.SessionID != s.config.ThreadID) ||
		(s.claude.id != "" && h.SessionID != s.claude.id) {
		r.Error = "Claude lifecycle identity conflicts with selected native state"
		s.claude.rejected = "identity_conflict"
		return r
	}
	s.claude.id = h.SessionID
	s.claude.lastHook = time.Now()
	if err = s.claude.accept(*h, s.inputGeneration); err != nil {
		r.Error = err.Error()
		s.claude.rejected = "invalid_lifecycle_transition"
	} else {
		s.claude.lastEvent, s.claude.rejected = h.Event, ""
		if h.Event == claudePromptSubmit {
			// Only the task's own turn can complete a task launch.
			s.claude.taskTurn = s.config.Task != "" && h.Prompt == s.config.Task
		}
	}
	return r
}
func (c *claudeLifecycle) accept(h claudeHook, input uint64) error {
	switch h.Event {
	case claudePromptSubmit:
		if len(c.pending) != 0 {
			return errors.New("claude prompt arrived with pending tools")
		}
		c.turn, c.input = turnRunning, input
		c.pending = map[string]string{}
	case claudePreTool:
		if c.turn != turnRunning || !validClaudeTool(h) || c.pending[h.ToolID] != "" {
			return errors.New("unsupported Claude tool start")
		}
		c.pending[h.ToolID] = h.ToolName
	case claudePostTool:
		if !validClaudeTool(h) || c.pending[h.ToolID] != h.ToolName {
			return errors.New("unmatched Claude tool completion")
		}
		delete(c.pending, h.ToolID)
	case claudePostToolFailure:
		// An interrupted call is outside the supported lifecycle and stays
		// pending.
		if !validClaudeTool(h) || c.pending[h.ToolID] != h.ToolName || h.Interrupt == nil || *h.Interrupt {
			return errors.New("unmatched or interrupted Claude tool failure")
		}
		delete(c.pending, h.ToolID)
	case claudeStop:
		if c.turn != turnRunning || len(c.pending) != 0 || h.StopActive == nil || *h.StopActive {
			return errors.New("claude turn has no safe Stop observation")
		}
		c.turn, c.everCompleted = turnCompleted, true
	case claudeStopFailure:
		// A repeated failure report of the same turn is accepted.
		if c.turn != turnRunning && c.turn != turnFailed {
			return errors.New("claude turn failure has no started turn")
		}
		// An API error ends the turn; no tool can still be running.
		c.turn, c.pending = turnFailed, map[string]string{}
	default:
		return errors.New("unsupported Claude lifecycle event")
	}
	return nil
}
func validClaudeTool(h claudeHook) bool {
	return harness.ClaudeSupportedTool(h.ToolName) && h.ToolID != "" && len(h.ToolID) <= 256 &&
		!strings.ContainsAny(h.ToolID, "\x00\r\n")
}

// claudeTurnless reports that a gated first visit has no Claude conversation
// to keep: no prompt was ever submitted, or every submitted turn ended on an
// API error before any turn completed. A failed transcript is not a
// resumable checkpoint, so it is left behind.
func (s *server) claudeTurnless() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.claude
	if c.turn == turnIdle {
		return c.id == ""
	}
	return c.turn == turnFailed && !c.everCompleted && len(c.pending) == 0
}

// claudeTurnReady reports a completed turn with no running tool and no
// unsettled input after it. Input after the completed turn blocks the stop
// until it settles: a submitted or queued prompt reports UserPromptSubmit and
// clears the completion, while terminal replies, focus reports and drafts do
// not. A task launch's completed turn must be the one its task's prompt
// started. The caller holds s.mu.
func (s *server) claudeTurnReady() bool {
	c := s.claude
	return c.turn == turnCompleted && len(c.pending) == 0 && (c.input == s.inputGeneration || s.inputSettled()) &&
		(s.config.Task == "" || c.taskTurn)
}

// claudeRestoredUnchanged reports a resumed visit in which no prompt was
// submitted: its selected conversation is the restored one, so a stop needs no
// new turn. Unsubmitted input must have settled. A task launch always needs
// its task's new turn. The caller holds s.mu.
func (s *server) claudeRestoredUnchanged() bool {
	c := s.claude
	return c.turn == turnIdle && s.config.ThreadID != "" && s.config.Task == "" && len(c.pending) == 0 &&
		(s.inputGeneration == 0 || s.inputSettled())
}

func (s *server) observeClaudeTurn() (string, string, error) {
	s.mu.Lock()
	c := s.claude
	selected, maxBytes := s.config.ThreadID, s.config.RolloutMaxBytes
	ready := s.claudeTurnReady()
	if s.claudeRestoredUnchanged() {
		// No prompt in this visit: the restored conversation is unchanged.
		ready, c.id = true, selected
	}
	s.mu.Unlock()
	if !ready || c.id == "" || (selected != "" && selected != c.id) {
		return "", "", errors.New("claude completed turn is unavailable")
	}
	path, err := harness.ClaudeProjectPath(workspace, c.id)
	if err != nil {
		return "", "", err
	}
	directories := []string{claudeHome, filepath.Join(claudeHome, "projects"),
		filepath.Dir(filepath.Join(claudeHome, path))}
	for _, directory := range directories {
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return "", "", errors.New("unsafe Claude native directory")
		}
	}
	name := filepath.Join(claudeHome, path)
	info, err := os.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBytes {
		return "", "", errors.New("claude selected transcript is unavailable")
	}
	f, err := os.Open(name)
	if err != nil {
		return "", "", err
	}
	data, readErr := io.ReadAll(io.LimitReader(f, maxBytes+1))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil {
		return "", "", errors.Join(readErr, closeErr)
	}
	state := harness.ClaudeState{ThreadID: c.id, Workspace: workspace, Path: path, Transcript: data}
	leaf, err := harness.ClaudeTranscriptLeaf(state, maxBytes)
	return c.id, leaf, err
}

// claudePreferences approves the gateway placeholder key when there is one.
// With none, Claude Code uses its own sign-in from the mounted
// .credentials.json, which /login creates (ADR 0047). Claude Code 2.1.318 and
// later ignore and drop bypassPermissionsModeAccepted; the settings file's
// skipDangerousModePermissionPrompt answers their confirmation instead. It
// stays for the older clients the movement proofs recorded.
func claudePreferences(env []string) map[string]any {
	preferences := map[string]any{"theme": "dark", "hasCompletedOnboarding": true,
		"bypassPermissionsModeAccepted": true,
		"projects":                      map[string]any{workspace: map[string]any{"hasTrustDialogAccepted": true}}}
	// The registry names the variable that carries the placeholder key.
	claude, _ := harness.Managed(harness.NativeClaude)
	for _, entry := range env {
		if key, ok := strings.CutPrefix(entry, claude.KeyEnv+"="); ok && key != "" {
			tail := key[max(0, len(key)-20):]
			preferences["customApiKeyResponses"] = map[string]any{"approved": []string{tail}, "rejected": []string{}}
		}
	}
	return preferences
}

func prepareClaudeLaunch(c config, _ string, env []string) ([]string, []string, error) {
	env = append(env, "CLAUDE_CONFIG_DIR="+claudeHome)
	if err := installClaudeJSON(filepath.Join(claudeHome, ".claude.json"), claudePreferences(env)); err != nil {
		return nil, nil, err
	}
	if err := installClaudeJSON(claudeSettings, harness.ClaudeManagedSettings(claudeHookCommand)); err != nil {
		return nil, nil, err
	}
	if err := installManagedClaudePlugin(claudePlugin); err != nil {
		return nil, nil, err
	}
	profile, err := loadProfile(c)
	if err != nil {
		return nil, nil, fmt.Errorf("profile: %w", err)
	}
	withSkills, err := installProfilePlugin(profilePlugin, profile)
	if err != nil {
		return nil, nil, err
	}
	args := claudeArguments(c, profile, withSkills)
	env = append(env,
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=false", "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1")
	return args, env, nil
}

// claudeArguments keeps every setting source disabled. Sproozi instructions
// follow the approved instructions, and the profile's follow those; skills and
// the control connection come only from the supervisor's plugins and MCP
// configuration.
func claudeArguments(c config, profile agentprofile.Profile, withSkills bool) []string {
	tools := claudeBash + ",Skill"
	args := []string{"--tools", tools, "--allowedTools", tools,
		"--strict-mcp-config", "--mcp-config", c.ClientConfig.ClaudeMCPConfig(), "--plugin-dir", claudePlugin}
	if withSkills {
		args = append(args, "--plugin-dir", profilePlugin)
	}
	args = append(args, "--setting-sources", "", "--settings", claudeSettings, "--permission-mode", "bypassPermissions",
		"--append-system-prompt", c.Instructions+"\n\n"+profileInstructions(profile, harness.NativeClaude))
	if c.Model != "" && !harness.NativeArgsSelectModel(harness.NativeClaude, c.Args) {
		args = append(args, "--model", c.Model)
	}
	args = append(args, c.Args...)
	if c.ThreadID != "" {
		args = append(args, "--resume", c.ThreadID)
	}
	if c.Task != "" {
		// The task is the one positional prompt, which the interactive client
		// submits at start. "--" keeps a task that starts with a dash literal.
		args = append(args, "--", c.Task)
	}
	return args
}

func installClaudeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	// These launch-generated files are never checkpoint-selected. Refuse an
	// existing settings/preference path supplied by a restored workspace/state.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if os.IsExist(err) {
		info, statErr := os.Lstat(path)
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			return errors.New("unsafe Claude launch settings")
		}
		existing, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(existing, data) {
			return errors.New("claude launch settings conflict")
		}
		return nil
	}
	if err != nil {
		return errors.New("fresh Claude launch settings are unavailable")
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	return errors.Join(err, f.Close())
}

// claudeHookMaxBytes bounds one hook payload. PostToolUse carries the tool's
// whole output, which Claude Code bounds (about 25,000 tokens for an MCP
// tool), and UserPromptSubmit a task of up to 32,768 characters. A payload
// past the bound fails the hook, which leaves the call pending and blocks
// completion rather than settling it.
const claudeHookMaxBytes = 4 << 20

// readClaudeHook returns as soon as one whole JSON object has arrived, not
// at the end of the input: the hook's report must not depend on when Claude
// Code closes the hook's stdin. Input that never completes an object holds
// the hook until Claude Code's hook timeout kills it; without an object there
// is nothing to report sooner. Bytes already read after the object, other
// than whitespace, refuse the payload. The bound counts every byte read: the
// object and whatever was read with it.
func readClaudeHook(input io.Reader) (*claudeHook, error) {
	counted := &countingReader{r: io.LimitReader(input, claudeHookMaxBytes+1)}
	decoder := json.NewDecoder(counted)
	var raw json.RawMessage
	err := decoder.Decode(&raw)
	if counted.n > claudeHookMaxBytes {
		return nil, errors.New("claude hook payload exceeds limit")
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, errors.New("claude hook payload is incomplete")
	}
	if err != nil {
		return nil, err
	}
	trailing, _ := io.ReadAll(decoder.Buffered())
	if len(bytes.TrimSpace(trailing)) != 0 {
		return nil, errors.New("claude hook payload has trailing data")
	}
	var h claudeHook
	// Pinned hook payloads also contain tool-input and tool-output fields.
	// They are opaque; only the selected lifecycle identity, the tool's name
	// and ID, and a submitted prompt (compared with a task) are consumed here.
	if err := json.Unmarshal(raw, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}
