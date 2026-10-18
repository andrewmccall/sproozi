package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	claudeUser      = "user"
	claudeAssistant = "assistant"
	claudeText      = "text"
)

var claudeSessionID = regexp.MustCompile(`^[0-9a-f]{8}(-[0-9a-f]{4}){3}-[0-9a-f]{12}$`)
var claudeProjectCharacters = regexp.MustCompile(`[^a-zA-Z0-9]`)
var claudeCacheHash = regexp.MustCompile(`^[0-9a-f]{64}$`)

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
	if !claudeSessionID.MatchString(threadID) || !filepath.IsAbs(workspace) ||
		filepath.Clean(workspace) != workspace || strings.ContainsRune(workspace, 0) {
		return "", errors.New("invalid Claude checkpoint selection")
	}
	project := claudeProjectCharacters.ReplaceAllString(workspace, "-")
	return "projects/" + project + "/" + threadID + ".jsonl", nil
}

// CaptureClaudeState selects exact bounded transcript bytes after the caller
// has independently stopped every native/workspace writer. It verifies a settled
// primary chain and resolved foreground Bash tools, not a live turn completion.
func CaptureClaudeState(home, workspace, threadID string, maxBytes int64) (ClaudeState, error) {
	path, err := ClaudeProjectPath(workspace, threadID)
	if err != nil || maxBytes <= 0 {
		return ClaudeState{}, errors.New("invalid Claude checkpoint selection")
	}
	if err = claudeDirectories(home, filepath.Dir(path), false); err != nil {
		return ClaudeState{}, err
	}
	name := filepath.Join(home, filepath.FromSlash(path))
	info, err := os.Lstat(name)
	if err != nil {
		return ClaudeState{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxBytes {
		return ClaudeState{}, errors.New("unsafe or oversized Claude transcript")
	}
	file, err := os.Open(name)
	if err != nil {
		return ClaudeState{}, err
	}
	data, readErr := io.ReadAll(io.LimitReader(file, maxBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return ClaudeState{}, errors.Join(readErr, closeErr)
	}
	state := ClaudeState{ThreadID: threadID, Workspace: workspace, Path: path, Transcript: data}
	if _, err = ClaudeTranscriptLeaf(state, maxBytes); err != nil {
		return ClaudeState{}, err
	}
	return state, nil
}

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
	if err = claudeDirectories(home, filepath.Dir(path), true); err != nil {
		return err
	}
	name := filepath.Join(home, filepath.FromSlash(path))
	file, err := os.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(state.Transcript)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(name)
		return errors.Join(writeErr, closeErr)
	}
	return nil
}

func claudeDirectories(home, relative string, create bool) error {
	dir := home
	for _, part := range append([]string{""}, strings.Split(filepath.ToSlash(relative), "/")...) {
		if part != "" {
			dir = filepath.Join(dir, part)
			if create {
				if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
					return err
				}
			}
		}
		info, err := os.Lstat(dir)
		if err != nil {
			return err
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe Claude native directory")
		}
	}
	return nil
}

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
	Data         struct {
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
	chain := claudeChain{pending: map[string]string{}, tools: map[string]bool{}, stops: map[string]bool{}, seen: map[string]bool{}}
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
	pending            map[string]string
	tools, stops, seen map[string]bool
	previous, leaf     string
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
		err = validateClaudeAttachment(row)
	case "system":
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
		valid = claudeCacheHash.MatchString(row.CacheHash) && len(row.Message.Content) != 0 &&
			(row.Message.Role == claudeUser || row.Message.Role == claudeAssistant)
	case "api-request":
		valid = row.Version != "" && claudeSessionID.MatchString(row.RequestID) && claudeCacheHash.MatchString(row.ShapeHash)
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
	case "environment", "model", "total_tokens_reminder", "session_context", "date", "remote_session_change", "prompt_snapshot":
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
	c.leaf = ""
	for _, block := range blocks {
		if err = validateClaudeBlock(row, block, c.pending, c.tools); err != nil {
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
		!path.IsAbs(row.Cwd) || (previous == "" && row.Type != claudeUser) {
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

func validateClaudeProgress(row claudeRecord, pending map[string]string, seen, stops map[string]bool, leaf string) error {
	data := row.Data
	if data.Type != "hook_progress" || data.Command == "" || row.ToolUseID == "" ||
		row.ParentTool != row.ToolUseID {
		return errors.New("unsupported Claude progress record")
	}
	switch data.Event {
	case "Stop":
		if data.Name != "Stop" || leaf == "" || len(pending) != 0 || stops[row.ToolUseID] {
			return errors.New("unmatched Claude stop hook progress")
		}
		stops[row.ToolUseID] = true
	case "PreToolUse":
		if data.Name != "PreToolUse:Bash" || pending[row.ToolUseID] == "" {
			return errors.New("unmatched Claude pre-tool hook progress")
		}
	case "PostToolUse":
		if data.Name != "PostToolUse:Bash" || !seen[row.ToolUseID] || pending[row.ToolUseID] != "" {
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

func validateClaudeBlock(row claudeRecord, block claudeBlock, pending map[string]string, seen map[string]bool) error {
	switch block.Type {
	case claudeText:
		if block.Text == "" {
			return errors.New("empty Claude text block")
		}
	case "tool_use":
		var input struct {
			Background bool `json:"run_in_background"`
		}
		if row.Type != claudeAssistant || block.ID == "" || seen[block.ID] || block.Name != "Bash" ||
			json.Unmarshal(block.Input, &input) != nil || input.Background {
			return errors.New("unsupported Claude tool call")
		}
		pending[block.ID], seen[block.ID] = row.UUID, true
	case "tool_result":
		var result struct {
			Interrupted *bool `json:"interrupted"`
		}
		if row.Type != claudeUser || pending[block.ToolUseID] == "" || block.IsError ||
			row.SourceTool != pending[block.ToolUseID] || json.Unmarshal(row.ToolResult, &result) != nil ||
			result.Interrupted == nil || *result.Interrupted {
			return errors.New("unmatched or unsupported Claude tool result")
		}
		delete(pending, block.ToolUseID)
	default:
		return fmt.Errorf("unsupported Claude content block %q", block.Type)
	}
	return nil
}
