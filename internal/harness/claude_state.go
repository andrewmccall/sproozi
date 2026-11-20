package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	posixpath "path"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	claudeUser      = "user"
	claudeAssistant = "assistant"
	claudeSystem    = "system"
	claudeText      = "text"
)

var claudeSessionID = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
var claudeProjectCharacters = regexp.MustCompile(`[^a-zA-Z0-9]`)
var claudeCacheHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

// claudeMCPTool is Claude Code's name for a tool of a configured MCP server:
// mcp__<server>__<tool>. Sproozi server names have no underscore, so the
// server part ends at the first double underscore.
var claudeMCPTool = regexp.MustCompile(`^mcp__[A-Za-z0-9-]{1,63}__[A-Za-z0-9_-]{1,128}$`)

// ClaudeBash is Claude Code's foreground shell tool.
const ClaudeBash = "Bash"

// ClaudeSupportedTool reports a tool the Claude lifecycle and transcript
// adapter pair: foreground Bash, or a tool of a configured MCP server such as
// the sproozi control connection's mcp__sproozi__diagnose (ADR 0011,
// clarification of 2026-11-14). Subagents, background tasks and every other
// native tool stay unsupported.
func ClaudeSupportedTool(name string) bool {
	return name == ClaudeBash || name == ClaudeSkill || claudeMCPTool.MatchString(name)
}

// ClaudeSkill is Claude Code's tool for loading a skill. Managed sessions load
// skills only from the supervisor's plugins: the sproozi plugin's
// sproozi-access and the profile plugin's skills.
const ClaudeSkill = "Skill"

// claudeManagedSkill is a skill name the Skill tool can load in a managed
// session, as the plugin namespace and the skill's directory name.
var claudeManagedSkill = regexp.MustCompile(`^(profile:[a-z0-9][a-z0-9-]{0,62}|sproozi:sproozi-access)$`)

// claudeSkillContent begins the row in which Claude Code injects a loaded
// skill's instructions after the Skill tool's result.
const claudeSkillContent = "Base directory for this skill: "

// ClaudeState selects one primary conversation transcript, excluding credential
// and settings files, indexes, shell snapshots and subagent state. Historical
// permissionMode fields remain opaque; fresh trusted launch options must select
// destination permissions independently, with native override behavior verified.
// Workspace selects the native project directory; historical cwd fields remain
// opaque and are not rewritten when the destination workspace changes.
// Native context and prompt snapshots remain history, not proof of approved
// instructions or completion. Both require independent trusted observations.
type ClaudeState struct {
	ThreadID   string
	Workspace  string
	Path       string
	Transcript []byte
}

// ClaudeProjectPath returns the exact project path exercised by the native
// client. Lookup never scans another project or adopts a different native ID.
func ClaudeProjectPath(workspace, threadID string) (string, error) {
	if !claudeSessionID.MatchString(threadID) || (!posixpath.IsAbs(workspace) && !filepath.IsAbs(workspace)) ||
		(posixpath.Clean(workspace) != workspace && filepath.Clean(workspace) != workspace) || strings.ContainsRune(workspace, 0) {
		return "", errors.New("invalid Claude checkpoint selection")
	}
	project := claudeProjectCharacters.ReplaceAllString(workspace, "-")
	return "projects/" + project + "/" + threadID + ".jsonl", nil
}

// CaptureClaudeState selects exact bounded transcript bytes after the caller
// has independently stopped every native/workspace writer. It verifies a settled
// primary chain and resolved foreground Bash tools, not a live turn completion.
// A final row the stopped writer did not finish is left out (stoppedJournal).
func CaptureClaudeState(home, workspace, threadID string, maxBytes int64) (ClaudeState, error) {
	path, err := ClaudeProjectPath(workspace, threadID)
	if err != nil || maxBytes <= 0 {
		return ClaudeState{}, errors.New("invalid Claude checkpoint selection")
	}
	if err = stateDirectories(home, filepath.Dir(path), false, claudeUnsafeDirectory); err != nil {
		return ClaudeState{}, err
	}
	name := filepath.Join(home, filepath.FromSlash(path))
	info, err := os.Lstat(name)
	if err != nil {
		return ClaudeState{}, stateFileError(err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxBytes {
		return ClaudeState{}, refusedState(errors.New("unsafe or oversized Claude transcript"))
	}
	file, err := os.Open(name)
	if err != nil {
		return ClaudeState{}, stateFileError(err)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return ClaudeState{}, stateFileError(errors.Join(readErr, closeErr))
	}
	state := ClaudeState{ThreadID: threadID, Workspace: workspace, Path: path, Transcript: stoppedJournal(data)}
	if _, err = ClaudeTranscriptLeaf(state, maxBytes); err != nil {
		return ClaudeState{}, refusedState(err)
	}
	return state, nil
}

// captureClaude is Claude Code's Harness.CaptureState: its transcript's path
// encodes the workspace.
func captureClaude(home, workspace, threadID string, maxBytes int64) (ConversationState, error) {
	return capturedState(CaptureClaudeState(home, workspace, threadID, maxBytes))
}

// Kind is NativeClaude.
func (s ClaudeState) Kind() NativeKind { return NativeClaude }

// Thread is the Claude Code session ID.
func (s ClaudeState) Thread() string { return s.ThreadID }

// Size is the transcript's length.
func (s ClaudeState) Size() int { return len(s.Transcript) }

// CompletedTurn is ClaudeTranscriptLeaf: Claude Code writes no completion
// event, so the settled leaf is the candidate turn.
func (s ClaudeState) CompletedTurn(maxBytes int64) (string, error) {
	return ClaudeTranscriptLeaf(s, maxBytes)
}

// Restore is RestoreClaudeState, at the destination workspace's project
// path.
func (s ClaudeState) Restore(home, workspace string, maxBytes int64) error {
	return RestoreClaudeState(home, workspace, s, maxBytes)
}

// FinalText is ClaudeFinalText.
func (s ClaudeState) FinalText(maxBytes int64) (string, error) { return ClaudeFinalText(s, maxBytes) }

// ValidateContinuity finds no evidence to compare: Claude Code's transcript
// is no proof of approved instructions, so the trusted launch supplies them
// fresh and holds them invariant (ADR 0011).
func (s ClaudeState) ValidateContinuity(string, int64) error { return nil }

// RestoreClaudeState publishes the same transcript bytes at the destination's
// exact native project path. Configuration and approved instructions are supplied
// independently by the destination. Existing transcripts are never overwritten.
func RestoreClaudeState(home, workspace string, state ClaudeState, maxBytes int64) error {
	if _, err := ClaudeTranscriptLeaf(state, maxBytes); err != nil {
		return err
	}
	path, err := ClaudeProjectPath(workspace, state.ThreadID)
	if err != nil {
		return err
	}
	if err = stateDirectories(home, filepath.Dir(path), true, claudeUnsafeDirectory); err != nil {
		return err
	}
	return writeStateFile(filepath.Join(home, filepath.FromSlash(path)), state.Transcript)
}

// claudeUnsafeDirectory refuses a link or non-directory on the transcript's
// path.
const claudeUnsafeDirectory = "unsafe Claude native directory"

type claudeRecord struct {
	Type           string          `json:"type"`
	Operation      string          `json:"operation"`
	SessionID      string          `json:"sessionId"`
	Version        string          `json:"version"`
	UUID           string          `json:"uuid"`
	ParentUUID     *string         `json:"parentUuid"`
	Sidechain      *bool           `json:"isSidechain"`
	Cwd            string          `json:"cwd"`
	SourceTool     string          `json:"sourceToolAssistantUUID"`
	ToolResult     json.RawMessage `json:"toolUseResult"`
	APIError       bool            `json:"isApiErrorMessage"`
	Subtype        string          `json:"subtype"`
	Mode           string          `json:"mode"`
	PermissionMode string          `json:"permissionMode"`
	Atis           string          `json:"atis"`
	LastPrompt     string          `json:"lastPrompt"`
	LeafUUID       string          `json:"leafUuid"`
	CacheHash      string          `json:"hash"`
	Tools          []string        `json:"tools"`
	ShapeHash      string          `json:"shapeHash"`
	RequestID      string          `json:"id"`
	Attachment     struct {
		Type string `json:"type"`
	} `json:"attachment"`
	RenderedRole string `json:"renderedRole"`
	DurationMS   *int64 `json:"durationMs"`
	MessageCount *int   `json:"messageCount"`
	ToolUseID    string `json:"toolUseID"`
	ParentTool   string `json:"parentToolUseID"`
	// IsMeta marks a row Claude Code adds rather than the user wrote, and
	// SourceToolUseID names the tool call it belongs to.
	IsMeta          *bool  `json:"isMeta"`
	SourceToolUseID string `json:"sourceToolUseID"`
	Data            struct {
		Type    string `json:"type"`
		Event   string `json:"hookEvent"`
		Name    string `json:"hookName"`
		Command string `json:"command"`
	} `json:"data"`
	MessageID string `json:"messageId"`
	Snapshot  struct {
		MessageID string                     `json:"messageId"`
		Backups   map[string]json.RawMessage `json:"trackedFileBackups"`
	} `json:"snapshot"`
	SnapshotUpdate *bool `json:"isSnapshotUpdate"`
	HookCount      int   `json:"hookCount"`
	HookInfos      []struct {
		Command string `json:"command"`
	} `json:"hookInfos"`
	HookErrors   []json.RawMessage `json:"hookErrors"`
	Prevented    *bool             `json:"preventedContinuation"`
	StopReason   string            `json:"stopReason"`
	HasOutput    *bool             `json:"hasOutput"`
	Level        string            `json:"level"`
	RetryError   json.RawMessage   `json:"error"`
	RetryAttempt *int              `json:"retryAttempt"`
	MaxRetries   *int              `json:"maxRetries"`
	RetryInMS    *float64          `json:"retryInMs"`
	Message      struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	Text      string          `json:"text"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// claudeNoContent is the text Claude Code records for a reply with no content.
const claudeNoContent = "(no content)"

// ClaudeFinalText returns the reply that ends at the selected transcript leaf:
// the text blocks of that final API message, in order, joined by newlines.
// Claude Code writes each content block of one reply as its own row with the
// same message ID. A reply Claude recorded as having no content is empty. The
// text is untrusted workload output, never completion or accounting evidence.
func ClaudeFinalText(state ClaudeState, maxBytes int64) (string, error) {
	leaf, err := ClaudeTranscriptLeaf(state, maxBytes)
	if err != nil {
		return "", err
	}
	type reply struct {
		message string
		text    []string
	}
	var replies []reply
	for line := range bytes.SplitSeq(state.Transcript, []byte{'\n'}) {
		var row struct {
			Type    string `json:"type"`
			UUID    string `json:"uuid"`
			Message struct {
				ID      string          `json:"id"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if len(line) == 0 || json.Unmarshal(line, &row) != nil || (row.Type != claudeUser && row.Type != claudeAssistant) {
			continue
		}
		if row.Type == claudeUser {
			replies = append(replies, reply{})
			continue
		}
		var blocks []claudeBlock
		_ = json.Unmarshal(row.Message.Content, &blocks)
		if n := len(replies); n == 0 || replies[n-1].message != row.Message.ID || row.Message.ID == "" {
			replies = append(replies, reply{message: row.Message.ID})
		}
		last := &replies[len(replies)-1]
		for _, block := range blocks {
			if block.Type == claudeText {
				last.text = append(last.text, block.Text)
			}
		}
		if row.UUID == leaf {
			text := strings.Join(last.text, "\n")
			if text == claudeNoContent {
				text = ""
			}
			return text, nil
		}
	}
	return "", errors.New("claude transcript has no settled final assistant leaf")
}

// ClaudeTranscriptLeaf returns a candidate settled assistant leaf UUID after
// validating the selected primary chain and paired supported tools. This is NOT a
// completed-turn observer: 2.1.27 writes assistant text with stop_reason:null,
// and native prompt snapshots cannot establish the trusted launch configuration.
func ClaudeTranscriptLeaf(state ClaudeState, maxBytes int64) (string, error) {
	path, err := ClaudeProjectPath(state.Workspace, state.ThreadID)
	if err != nil || path != state.Path || maxBytes <= 0 || len(state.Transcript) == 0 ||
		int64(len(state.Transcript)) > maxBytes || !bytes.HasSuffix(state.Transcript, []byte{'\n'}) {
		return "", errors.New("invalid or oversized Claude checkpoint")
	}
	chain := claudeChain{pending: map[string]string{}, tools: map[string]string{}, skills: map[string]string{}, stops: map[string]bool{}, seen: map[string]bool{}}
	for line := range bytes.SplitSeq(state.Transcript, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		if err = chain.accept(line, state.ThreadID); err != nil {
			return "", err
		}
	}
	if chain.leaf == "" || len(chain.pending) != 0 {
		return "", errors.New("claude transcript has no settled final assistant leaf")
	}
	return chain.leaf, nil
}

type claudeChain struct {
	// pending maps a running tool's ID to the row that started it; tools maps
	// every started tool's ID to its name.
	pending, tools map[string]string
	// skills maps a Skill call's ID to the skill it loads. loading is the
	// Skill call whose result arrived and whose instructions row has not;
	// that call stays pending until the row arrives. permissions allows the
	// one command_permissions attachment that follows those instructions.
	skills         map[string]string
	loading        string
	permissions    bool
	stops, seen    map[string]bool
	previous, leaf string
}

func (c *claudeChain) accept(line []byte, id string) error {
	var row claudeRecord
	if err := json.Unmarshal(line, &row); err != nil {
		return errors.New("malformed or foreign Claude transcript")
	}
	if row.Type == "file-history-snapshot" {
		c.leaf = ""
		return validateClaudeSnapshot(row)
	}
	if row.SessionID != id {
		return errors.New("foreign Claude transcript")
	}
	if metadata, err := validateClaudeMetadata(row); metadata {
		return err
	}
	if row.Type == "queue-operation" {
		if row.Operation != "dequeue" {
			return errors.New("unsupported Claude queue operation")
		}
		c.leaf = ""
		return nil
	}
	if err := validateClaudeRecord(row, c.previous, c.seen); err != nil {
		return err
	}
	var err error
	switch row.Type {
	case claudeUser, claudeAssistant:
		err = c.message(row)
	case "progress":
		err = validateClaudeProgress(row, c.pending, c.tools, c.stops, c.leaf)
	case "attachment":
		if row.Attachment.Type == "command_permissions" {
			// It follows a loaded skill's instructions, once.
			if !c.permissions {
				return errors.New("unsupported Claude context attachment")
			}
			c.permissions = false
			row.Attachment.Type = "session_context"
		}
		err = validateClaudeAttachment(row)
	case claudeSystem:
		switch row.Subtype {
		case "stop_hook_summary":
			if c.leaf == "" || len(c.pending) != 0 {
				err = errors.New("stop summary has no settled Claude assistant leaf")
			} else {
				err = validateClaudeSummary(row, c.stops)
			}
		case "turn_duration":
			if c.leaf == "" || len(c.pending) != 0 || row.DurationMS == nil || *row.DurationMS < 0 ||
				row.MessageCount == nil || *row.MessageCount < 0 {
				err = errors.New("unsupported Claude turn duration")
			}
		case "api_error":
			err = validateClaudeRetry(row, c.pending)
			c.leaf = ""
		default:
			err = errors.New("unsupported Claude system record")
		}
	default:
		return errors.New("unsupported Claude transcript record")
	}
	if err != nil {
		return err
	}
	c.seen[row.UUID], c.previous = true, row.UUID
	return nil
}

// These native caches are outside the primary message chain. They neither
// change its leaf nor supply destination configuration or completion evidence.
func validateClaudeMetadata(row claudeRecord) (bool, error) {
	valid := false
	messageCache := false
	switch row.Type {
	case "mode":
		valid = row.Mode == "normal"
	case "permission-mode":
		valid = row.PermissionMode == "bypassPermissions"
	case "atis-latch":
		valid = row.Atis == ""
	case "last-prompt":
		valid = row.LastPrompt != "" && claudeSessionID.MatchString(row.LeafUUID)
	case "api-request-shape":
		valid = row.Version != "" && claudeCacheHash.MatchString(row.ShapeHash)
	case "api-request-blob":
		messageCache = true
		// Claude Code 2.1.302 also caches the system prompt's request blocks
		// with role "system" once a conversation has a second request.
		valid = claudeCacheHash.MatchString(row.CacheHash) && len(row.Message.Content) != 0 &&
			(row.Message.Role == claudeUser || row.Message.Role == claudeAssistant || row.Message.Role == claudeSystem)
	case "api-request":
		valid = row.Version != "" && claudeSessionID.MatchString(row.RequestID) && claudeCacheHash.MatchString(row.ShapeHash)
	case "tools-used":
		// Claude Code 2.1.318 lists the tools a conversation has used so far.
		// Each must be one the adapter supports.
		valid = len(row.Tools) != 0
		for _, name := range row.Tools {
			valid = valid && ClaudeSupportedTool(name)
		}
	default:
		return false, nil
	}
	if !valid || row.UUID != "" || row.ParentUUID != nil || row.Sidechain != nil ||
		(!messageCache && (row.Message.Role != "" || len(row.Message.Content) != 0)) || row.ToolUseID != "" || row.APIError {
		return true, errors.New("unsupported Claude session metadata")
	}
	return true, nil
}

func validateClaudeAttachment(row claudeRecord) error {
	if row.Message.Role != "" || len(row.Message.Content) != 0 || row.ToolUseID != "" ||
		(row.RenderedRole != "" && row.RenderedRole != "user" && row.RenderedRole != "system") {
		return errors.New("unsupported Claude context attachment")
	}
	switch row.Attachment.Type {
	// skill_listing appears once any plugin skill is loaded, as the Sproozi
	// integration always loads one.
	case "environment", "model", "total_tokens_reminder", "session_context", "date", "remote_session_change", "prompt_snapshot",
		"skill_listing":
		return nil
	default:
		return errors.New("unsupported Claude context attachment")
	}
}
func (c *claudeChain) message(row claudeRecord) error {
	blocks, err := claudeContent(row)
	if err != nil {
		return err
	}
	c.leaf, c.permissions = "", false
	if (row.IsMeta != nil && *row.IsMeta) || row.SourceToolUseID != "" || c.loading != "" {
		return c.skillContent(row, blocks)
	}
	for _, block := range blocks {
		if err = c.block(row, block); err != nil {
			return err
		}
		if row.Type == claudeAssistant && block.Type == claudeText && block.Text != "" {
			c.leaf = row.UUID
		}
	}
	return nil
}

func validateClaudeRecord(row claudeRecord, previous string, seen map[string]bool) error {
	parent := ""
	if row.ParentUUID != nil {
		parent = *row.ParentUUID
	}
	toolParent := row.Type == claudeUser && row.SourceTool != "" &&
		parent == row.SourceTool && seen[parent]
	message := row.Type == claudeUser || row.Type == claudeAssistant
	if (message && row.Message.Role != row.Type) || row.APIError ||
		row.Version == "" || row.Sidechain == nil || *row.Sidechain ||
		!claudeSessionID.MatchString(row.UUID) || seen[row.UUID] || (parent != previous && !toolParent) ||
		(!posixpath.IsAbs(row.Cwd) && !filepath.IsAbs(row.Cwd)) || (previous == "" && row.Type != claudeUser) {
		return errors.New("incompatible Claude primary transcript chain")
	}
	return nil
}

func validateClaudeSnapshot(row claudeRecord) error {
	if !claudeSessionID.MatchString(row.MessageID) || row.Snapshot.MessageID != row.MessageID ||
		row.Snapshot.Backups == nil || len(row.Snapshot.Backups) != 0 ||
		row.SnapshotUpdate == nil || *row.SnapshotUpdate || row.SessionID != "" || row.UUID != "" {
		return errors.New("unsupported Claude file history snapshot")
	}
	return nil
}

func validateClaudeProgress(row claudeRecord, pending, tools map[string]string, stops map[string]bool, leaf string) error {
	data := row.Data
	if data.Type != "hook_progress" || data.Command == "" || row.ToolUseID == "" ||
		row.ParentTool != row.ToolUseID {
		return errors.New("unsupported Claude progress record")
	}
	switch data.Event {
	case claudeStopEvent:
		if data.Name != claudeStopEvent || leaf == "" || len(pending) != 0 || stops[row.ToolUseID] {
			return errors.New("unmatched Claude stop hook progress")
		}
		stops[row.ToolUseID] = true
	case "PreToolUse":
		if data.Name != "PreToolUse:"+tools[row.ToolUseID] || pending[row.ToolUseID] == "" {
			return errors.New("unmatched Claude pre-tool hook progress")
		}
	case "PostToolUse", "PostToolUseFailure":
		if data.Name != data.Event+":"+tools[row.ToolUseID] || tools[row.ToolUseID] == "" || pending[row.ToolUseID] != "" {
			return errors.New("unmatched Claude post-tool hook progress")
		}
	default:
		return errors.New("unsupported Claude hook event")
	}
	return nil
}

func validateClaudeSummary(row claudeRecord, stops map[string]bool) error {
	if row.Subtype != "stop_hook_summary" || !claudeSessionID.MatchString(row.ToolUseID) ||
		(len(stops) != 0 && !stops[row.ToolUseID]) || row.HookCount != 1 ||
		len(row.HookInfos) != 1 || row.HookInfos[0].Command == "" || row.HookErrors == nil ||
		len(row.HookErrors) != 0 || row.Prevented == nil || *row.Prevented || row.StopReason != "" ||
		row.HasOutput == nil || *row.HasOutput || row.Level != "suggestion" {
		return errors.New("unsupported Claude stop hook summary")
	}
	delete(stops, row.ToolUseID)
	return nil
}

// The native TUI records a recoverable 429 while another conservative gateway
// reservation is active. This is opaque history, never accounting evidence or
// completion. Its primary-chain successor must establish a new assistant leaf.
func validateClaudeRetry(row claudeRecord, pending map[string]string) error {
	if row.Level != "error" || row.RetryAttempt == nil || *row.RetryAttempt < 1 ||
		row.MaxRetries == nil || *row.MaxRetries != 10 || *row.RetryAttempt > *row.MaxRetries ||
		row.RetryInMS == nil || *row.RetryInMS < 0 || *row.RetryInMS > 60000 || len(pending) != 0 ||
		len(row.RetryError) == 0 || len(row.RetryError) > 8192 {
		return errors.New("unsupported Claude API retry record")
	}
	var detail struct {
		Status    int                        `json:"status"`
		Headers   map[string]json.RawMessage `json:"headers"`
		RequestID json.RawMessage            `json:"requestID"`
		Error     struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"error"`
	}
	decoder := json.NewDecoder(bytes.NewReader(row.RetryError))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&detail) != nil || detail.Status != 429 || detail.Headers == nil || len(detail.Headers) != 0 ||
		!bytes.Equal(detail.RequestID, []byte("null")) || detail.Error.Type != "error" ||
		detail.Error.Error.Type != "budget_error" || detail.Error.Error.Message == "" ||
		len(detail.Error.Error.Message) > 4096 {
		return errors.New("unsupported Claude API retry error")
	}
	return nil
}

func claudeContent(row claudeRecord) ([]claudeBlock, error) {
	var text string
	if row.Type == claudeUser && json.Unmarshal(row.Message.Content, &text) == nil && text != "" && row.SourceTool == "" {
		return []claudeBlock{{Type: claudeText, Text: text}}, nil
	}
	var blocks []claudeBlock
	if err := json.Unmarshal(row.Message.Content, &blocks); err != nil || len(blocks) == 0 {
		return nil, errors.New("unsupported Claude message content")
	}
	if row.SourceTool != "" && (len(blocks) != 1 || blocks[0].Type != "tool_result") {
		return nil, errors.New("invalid Claude source tool reference")
	}
	return blocks, nil
}

// claudeInterrupted marks a tool call the user interrupted; an interrupted
// turn is outside the supported lifecycle.
const claudeInterrupted = "[Request interrupted"

// claudeSettledResult reports a supported tool's finished result. A successful
// Bash result records that it was not interrupted; a successful MCP result
// records the server's content. A failed call, such as a command that exited
// non-zero or an MCP tool that reported an error, records Claude Code's
// "Error: " text and is settled too, unless the user interrupted it.
func claudeSettledResult(name string, block claudeBlock, raw json.RawMessage) bool {
	if block.IsError {
		var text string
		return json.Unmarshal(raw, &text) == nil && strings.HasPrefix(text, "Error: ") &&
			!strings.Contains(text, claudeInterrupted) && !bytes.Contains(block.Content, []byte(claudeInterrupted))
	}
	if name == ClaudeBash {
		var result struct {
			Interrupted *bool `json:"interrupted"`
		}
		return json.Unmarshal(raw, &result) == nil && result.Interrupted != nil && !*result.Interrupted
	}
	var content any
	if json.Unmarshal(raw, &content) != nil {
		return false
	}
	switch content.(type) {
	case []any, string:
		return true
	}
	return false
}

// claudeSkillInput returns the skill a Skill call loads. The captured call
// names only the skill; a managed session's skills come from the sproozi and
// profile plugins.
func claudeSkillInput(raw json.RawMessage) (string, bool) {
	var input struct {
		Skill string `json:"skill"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || !claudeManagedSkill.MatchString(input.Skill) {
		return "", false
	}
	return input.Skill, true
}

// claudeSkillLaunched reports the Skill tool's successful result for skill:
// Claude Code launches the skill and then injects its instructions.
func claudeSkillLaunched(skill string, block claudeBlock, raw json.RawMessage) bool {
	var content string
	var result struct {
		Success     bool   `json:"success"`
		CommandName string `json:"commandName"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return !block.IsError && json.Unmarshal(block.Content, &content) == nil && content == "Launching skill: "+skill &&
		decoder.Decode(&result) == nil && result.Success && result.CommandName == skill
}

// skillContent accepts the row in which Claude Code injects the instructions
// of the skill whose Skill call just returned, which settles that call. Any
// other row Claude Code marks as its own stays unsupported.
func (c *claudeChain) skillContent(row claudeRecord, blocks []claudeBlock) error {
	if row.Type != claudeUser || row.IsMeta == nil || !*row.IsMeta || c.loading == "" ||
		row.SourceToolUseID != c.loading || row.SourceTool != "" || len(blocks) != 1 ||
		blocks[0].Type != claudeText || !strings.HasPrefix(blocks[0].Text, claudeSkillContent) {
		return errors.New("unsupported Claude skill instructions")
	}
	delete(c.pending, c.loading)
	c.loading, c.permissions = "", true
	return nil
}

func (c *claudeChain) block(row claudeRecord, block claudeBlock) error {
	switch block.Type {
	case claudeText:
		if block.Text == "" {
			return errors.New("empty Claude text block")
		}
	case "tool_use":
		var input struct {
			Background bool `json:"run_in_background"`
		}
		if row.Type != claudeAssistant || block.ID == "" || c.tools[block.ID] != "" || !ClaudeSupportedTool(block.Name) ||
			json.Unmarshal(block.Input, &input) != nil || input.Background || !bytes.HasPrefix(block.Input, []byte{'{'}) {
			return errors.New("unsupported Claude tool call")
		}
		if block.Name == ClaudeSkill {
			skill, ok := claudeSkillInput(block.Input)
			if !ok {
				return errors.New("unsupported Claude tool call")
			}
			c.skills[block.ID] = skill
		}
		c.pending[block.ID], c.tools[block.ID] = row.UUID, block.Name
	case "tool_result":
		name := c.tools[block.ToolUseID]
		if row.Type != claudeUser || c.pending[block.ToolUseID] == "" || row.SourceTool != c.pending[block.ToolUseID] {
			return errors.New("unmatched or unsupported Claude tool result")
		}
		if name == ClaudeSkill {
			if !claudeSkillLaunched(c.skills[block.ToolUseID], block, row.ToolResult) {
				return errors.New("unmatched or unsupported Claude tool result")
			}
			// The call settles when the skill's instructions arrive.
			c.loading = block.ToolUseID
			return nil
		}
		if !claudeSettledResult(name, block, row.ToolResult) {
			return errors.New("unmatched or unsupported Claude tool result")
		}
		delete(c.pending, block.ToolUseID)
	default:
		return fmt.Errorf("unsupported Claude content block %q", block.Type)
	}
	return nil
}
