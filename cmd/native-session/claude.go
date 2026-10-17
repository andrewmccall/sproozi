package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/andrewmccall/sproozi/internal/harness"
)

const (
	journalUnavailable = "journal_unavailable_or_invalid"
	claudePromptSubmit = "UserPromptSubmit"
	claudePreTool      = "PreToolUse"
	claudePostTool     = "PostToolUse"
	claudeStop         = "Stop"
	claudeBash         = "Bash"
	claudeHooksKey     = "hooks"

	opClaudeHook     = "observe-claude"
	claudeSettings   = "/tmp/sproozi-native/claude-settings.json"
	claudeHome       = "/workspace/session/claude"
	claudeExecutable = "/usr/local/bin/claude"
)

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
}
type claudeLifecycle struct {
	id        string
	started   bool
	completed bool
	input     uint64
	pending   map[string]bool
	lastEvent string
	rejected  string
}

// Completion diagnostics describe the observer's state without disclosing
// prompts, tool arguments, native journal bytes or selected filesystem paths.
type claudeCompletion struct {
	Started          bool   `json:"started"`
	Completed        bool   `json:"completed"`
	PendingTools     int    `json:"pendingTools"`
	PromptGeneration uint64 `json:"promptGeneration"`
	InputGeneration  uint64 `json:"inputGeneration"`
	LastEvent        string `json:"lastEvent,omitempty"`
	Rejected         string `json:"rejected,omitempty"`
	Observation      string `json:"observation"`
}

func (s *server) claudeCompletion() *claudeCompletion {
	s.mu.Lock()
	c := s.claude
	d := &claudeCompletion{Started: c.started, Completed: c.completed, PendingTools: len(c.pending),
		PromptGeneration: c.input, InputGeneration: s.inputGeneration,
		LastEvent: c.lastEvent, Rejected: c.rejected, Observation: "lifecycle_incomplete"}
	selected := s.config.ThreadID
	s.mu.Unlock()
	switch {
	case d.PendingTools != 0:
		d.Observation = "pending_tools"
	case d.PromptGeneration != d.InputGeneration:
		d.Observation = "input_changed"
	case c.id == "" || (selected != "" && c.id != selected):
		d.Observation = "identity_unavailable"
	case d.Completed:
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
	r := response{Op: opClaudeHook, State: s.state}
	if h == nil || s.config.Harness != harness.NativeClaude ||
		(s.state != stateActive && s.state != stateStopping) {
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
	if err = s.claude.accept(*h, s.inputGeneration); err != nil {
		r.Error = err.Error()
		s.claude.rejected = "invalid_lifecycle_transition"
	} else {
		s.claude.lastEvent, s.claude.rejected = h.Event, ""
	}
	return r
}
func (c *claudeLifecycle) accept(h claudeHook, input uint64) error {
	switch h.Event {
	case claudePromptSubmit:
		if len(c.pending) != 0 {
			return errors.New("claude prompt arrived with pending tools")
		}
		c.started, c.completed, c.input = true, false, input
		c.pending = map[string]bool{}
	case claudePreTool:
		if !c.started || c.completed || !validClaudeTool(h) || c.pending[h.ToolID] {
			return errors.New("unsupported Claude tool start")
		}
		c.pending[h.ToolID] = true
	case claudePostTool:
		if !validClaudeTool(h) || !c.pending[h.ToolID] {
			return errors.New("unmatched Claude tool completion")
		}
		delete(c.pending, h.ToolID)
	case claudeStop:
		if !c.started || len(c.pending) != 0 || h.StopActive == nil || *h.StopActive {
			return errors.New("claude turn has no safe Stop observation")
		}
		c.completed = true
	default:
		return errors.New("unsupported Claude lifecycle event")
	}
	return nil
}
func validClaudeTool(h claudeHook) bool {
	return h.ToolName == claudeBash && h.ToolID != "" && len(h.ToolID) <= 256 &&
		!strings.ContainsAny(h.ToolID, "\x00\r\n")
}

func (s *server) observeClaudeTurn() (string, string, error) {
	s.mu.Lock()
	c := s.claude
	selected, maxBytes := s.config.ThreadID, s.config.RolloutMaxBytes
	ready := c.completed && len(c.pending) == 0 && c.input == s.inputGeneration
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

func prepareClaudeLaunch(c config, env []string) ([]string, []string, error) {
	key := ""
	for _, entry := range env {
		if after, ok := strings.CutPrefix(entry, "ANTHROPIC_API_KEY="); ok {
			key = after
		}
	}
	if key == "" {
		return nil, nil, errors.New("claude requires a fresh gateway API placeholder")
	}
	tail := key
	if len(tail) > 20 {
		tail = tail[len(tail)-20:]
	}
	preferences := map[string]any{"theme": "dark", "hasCompletedOnboarding": true,
		"bypassPermissionsModeAccepted": true,
		"customApiKeyResponses":         map[string]any{"approved": []string{tail}, "rejected": []string{}},
		"projects":                      map[string]any{workspace: map[string]any{"hasTrustDialogAccepted": true}}}
	if err := installClaudeJSON(filepath.Join(claudeHome, ".claude.json"), preferences); err != nil {
		return nil, nil, err
	}
	hooks := map[string]any{}
	for _, event := range []string{claudePromptSubmit, claudePreTool, claudePostTool, claudeStop} {
		hooks[event] = []any{map[string]any{"matcher": "", claudeHooksKey: []any{map[string]any{
			"type": "command", "command": "/usr/local/bin/native-session observe-claude", "timeout": 5}}}}
	}
	if err := installClaudeJSON(claudeSettings, map[string]any{claudeHooksKey: hooks}); err != nil {
		return nil, nil, err
	}
	args := []string{"--tools", claudeBash, "--allowedTools", claudeBash,
		"--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
		"--setting-sources", "", "--settings", claudeSettings, "--permission-mode", "bypassPermissions",
		"--append-system-prompt", c.Instructions}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	if c.ThreadID != "" {
		args = append(args, "--resume", c.ThreadID)
	}
	env = append(env,
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION=false", "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS=1")
	return args, env, nil
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

func readClaudeHook(input io.Reader) (*claudeHook, error) {
	data, err := io.ReadAll(io.LimitReader(input, 65537))
	if err != nil || len(data) > 65536 {
		return nil, errors.New("claude hook payload exceeds limit")
	}
	var h claudeHook
	// Pinned hook payloads also contain prompt and tool-input fields. They are
	// opaque; only the exact selected lifecycle identity is consumed here.
	if err = json.Unmarshal(data, &h); err != nil {
		return nil, err
	}
	return &h, nil
}
