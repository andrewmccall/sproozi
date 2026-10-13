package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"mime"
	"net/http"
	"strings"
)

const (
	anthropicMessagesPath = "/v1/messages"
	eventStreamMediaType  = "text/event-stream"
	anthropicOutputTokens = "output_tokens"
)

// anthropicUsage contains validated, disjoint token categories. Anthropic's
// ordinary input count excludes tokens read from or written to the cache.
type anthropicUsage struct {
	Input, CacheRead, CacheWrite5m, CacheWrite1h, Output, Total int64
}

func validateAnthropicRequest(r *http.Request, body []byte, costCapped bool) error {
	if r.Method != http.MethodPost || r.URL.Path != anthropicMessagesPath ||
		(r.URL.RawQuery != "" && r.URL.RawQuery != "beta=true") {
		return fmt.Errorf("unsupported Anthropic operation")
	}
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil || request == nil {
		return fmt.Errorf("invalid Anthropic request")
	}
	var model string
	if json.Unmarshal(request["model"], &model) != nil || strings.TrimSpace(model) == "" {
		return fmt.Errorf("anthropic: model is required")
	}
	maxTokens, err := anthropicCount(request["max_tokens"], true)
	if err != nil || maxTokens <= 0 {
		return fmt.Errorf("positive Anthropic max_tokens is required")
	}
	if stream, ok := request["stream"]; ok {
		var streaming bool
		if bytes.Equal(bytes.TrimSpace(stream), []byte("null")) || json.Unmarshal(stream, &streaming) != nil {
			return fmt.Errorf("invalid Anthropic stream selector")
		}
	}
	if !costCapped {
		return nil
	}
	for key, allowed := range map[string]string{"service_tier": "standard_only", "speed": "standard", "inference_geo": "global"} {
		if raw, present := request[key]; present {
			var value string
			if json.Unmarshal(raw, &value) != nil || value != allowed {
				return fmt.Errorf("anthropic: request has unsupported billable options")
			}
		}
	}
	for _, key := range []string{"container", "mcp_servers", "fallback"} {
		if _, present := request[key]; present {
			return fmt.Errorf("anthropic: request has unsupported billable options")
		}
	}
	if raw, present := request["tools"]; present {
		var tools []map[string]json.RawMessage
		if json.Unmarshal(raw, &tools) != nil {
			return fmt.Errorf("invalid Anthropic tools")
		}
		for _, tool := range tools {
			if rawType, present := tool["type"]; present {
				var toolType string
				if json.Unmarshal(rawType, &toolType) != nil || toolType != "custom" {
					return fmt.Errorf("anthropic: server tools have no administrator pricing")
				}
			}
		}
	}
	return validateAnthropicCacheControls(body)
}

// Inspect protocol metadata only. Tool input and JSON schemas may contain
// arbitrary keys named cache_control; those do not select provider caching.
func validateAnthropicCacheControls(body []byte) error {
	var request struct {
		CacheControl json.RawMessage `json:"cache_control"`
		System       json.RawMessage `json:"system"`
		Tools        json.RawMessage `json:"tools"`
		Messages     []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &request) != nil {
		return fmt.Errorf("invalid Anthropic request")
	}
	if err := validateAnthropicCacheControl(request.CacheControl); err != nil {
		return err
	}
	for _, blocks := range []json.RawMessage{request.System, request.Tools} {
		if err := validateAnthropicCacheBlocks(blocks); err != nil {
			return err
		}
	}
	for _, message := range request.Messages {
		if err := validateAnthropicCacheBlocks(message.Content); err != nil {
			return err
		}
	}
	return nil
}

func validateAnthropicCacheControl(raw json.RawMessage) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var control map[string]json.RawMessage
	if json.Unmarshal(raw, &control) != nil || control == nil {
		return fmt.Errorf("invalid Anthropic cache control")
	}
	if ttl, present := control["ttl"]; present {
		var value string
		if json.Unmarshal(ttl, &value) != nil || value != "5m" {
			return fmt.Errorf("anthropic: cache TTL is unsupported under cost caps")
		}
	}
	return nil
}

func validateAnthropicCacheBlocks(raw json.RawMessage) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || bytes.HasPrefix(bytes.TrimSpace(raw), []byte("\"")) {
		return nil
	}
	var blocks []struct {
		Type         string          `json:"type"`
		CacheControl json.RawMessage `json:"cache_control"`
		Content      json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return fmt.Errorf("invalid Anthropic content blocks")
	}
	for _, block := range blocks {
		if err := validateAnthropicCacheControl(block.CacheControl); err != nil {
			return err
		}
		if block.Type == "tool_result" {
			if err := validateAnthropicCacheBlocks(block.Content); err != nil {
				return err
			}
		}
	}
	return nil
}

func decodeAnthropicUsage(body []byte, streaming bool, contentType string) (anthropicUsage, error) {
	if streaming {
		mediaType, _, err := mime.ParseMediaType(contentType)
		if strings.TrimSpace(contentType) != "" && (err != nil || mediaType != eventStreamMediaType) {
			return anthropicUsage{}, fmt.Errorf("unsupported Anthropic streaming response")
		}
		return decodeAnthropicStream(body)
	}
	var message anthropicMessage
	if err := json.Unmarshal(body, &message); err != nil {
		return anthropicUsage{}, fmt.Errorf("invalid Anthropic response")
	}
	if !message.valid() || !validAnthropicStopReason(message.StopReason) {
		return anthropicUsage{}, fmt.Errorf("incomplete Anthropic message")
	}
	return parseAnthropicUsage(message.Usage)
}

type anthropicMessage struct {
	Type       string          `json:"type"`
	Role       string          `json:"role"`
	Model      string          `json:"model"`
	StopReason string          `json:"stop_reason"`
	Usage      json.RawMessage `json:"usage"`
}

func (m anthropicMessage) valid() bool {
	return m.Type == "message" && m.Role == "assistant" && strings.TrimSpace(m.Model) != ""
}

func validAnthropicStopReason(reason string) bool {
	switch reason {
	case "end_turn", "max_tokens", "stop_sequence", "tool_use", "pause_turn", "refusal", "model_context_window_exceeded":
		return true
	default:
		return false
	}
}

func anthropicCount(raw json.RawMessage, required bool) (int64, error) {
	if !required && (len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))) {
		return 0, nil
	}
	var value int64
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil || value < 0 {
		return 0, fmt.Errorf("invalid Anthropic token count")
	}
	return value, nil
}

func parseAnthropicUsage(raw json.RawMessage) (anthropicUsage, error) {
	var fields map[string]json.RawMessage
	var usage anthropicUsage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return usage, fmt.Errorf("anthropic: usage is required")
	}
	for key, target := range map[string]*int64{
		"input_tokens": &usage.Input, anthropicOutputTokens: &usage.Output,
		"cache_read_input_tokens": &usage.CacheRead, "cache_creation_input_tokens": &usage.CacheWrite5m,
	} {
		value, err := anthropicCount(fields[key], key == "input_tokens" || key == anthropicOutputTokens)
		if err != nil {
			return anthropicUsage{}, err
		}
		*target = value
	}
	if rawCreation, present := fields["cache_creation"]; present && !bytes.Equal(bytes.TrimSpace(rawCreation), []byte("null")) {
		var creation map[string]json.RawMessage
		if json.Unmarshal(rawCreation, &creation) != nil || creation == nil {
			return anthropicUsage{}, fmt.Errorf("invalid Anthropic cache creation")
		}
		fiveMinutes, err := anthropicCount(creation["ephemeral_5m_input_tokens"], true)
		if err != nil {
			return anthropicUsage{}, err
		}
		oneHour, err := anthropicCount(creation["ephemeral_1h_input_tokens"], true)
		if err != nil {
			return anthropicUsage{}, err
		}
		if _, present := fields["cache_creation_input_tokens"]; !present || fiveMinutes > math.MaxInt64-oneHour || fiveMinutes+oneHour != usage.CacheWrite5m {
			return anthropicUsage{}, fmt.Errorf("inconsistent Anthropic cache creation")
		}
		usage.CacheWrite5m, usage.CacheWrite1h = fiveMinutes, oneHour
	}
	return totalAnthropicUsage(usage)
}

func totalAnthropicUsage(usage anthropicUsage) (anthropicUsage, error) {
	usage.Total = 0
	for _, count := range []int64{usage.Input, usage.CacheRead, usage.CacheWrite5m, usage.CacheWrite1h, usage.Output} {
		if count < 0 || count > math.MaxInt64-usage.Total {
			return anthropicUsage{}, fmt.Errorf("anthropic: token usage overflow")
		}
		usage.Total += count
	}
	return usage, nil
}

func decodeAnthropicStream(body []byte) (anthropicUsage, error) {
	normalized := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	if !bytes.HasSuffix(normalized, []byte("\n\n")) || bytes.Contains(normalized, []byte("\r")) {
		return anthropicUsage{}, fmt.Errorf("truncated Anthropic event stream")
	}
	var usage anthropicUsage
	var usageFields map[string]json.RawMessage
	type streamPhase uint8
	const (
		beforeMessage streamPhase = iota
		readingMessage
		completedMessage
		stoppedMessage
	)
	phase := beforeMessage
	for frame := range bytes.SplitSeq(normalized, []byte("\n\n")) {
		event, err := parseAnthropicFrame(frame)
		if err != nil {
			return anthropicUsage{}, err
		}
		if event == nil {
			continue
		}
		if phase == stoppedMessage {
			return anthropicUsage{}, fmt.Errorf("invalid Anthropic event stream")
		}
		switch event.Type {
		case errorEventType:
			return anthropicUsage{}, fmt.Errorf("unsuccessful Anthropic event stream")
		case "message_start":
			if phase != beforeMessage || !event.Message.valid() || event.Message.StopReason != "" {
				return anthropicUsage{}, fmt.Errorf("invalid Anthropic message start")
			}
			var err error
			usage, err = parseAnthropicUsage(event.Message.Usage)
			if err != nil {
				return anthropicUsage{}, err
			}
			if err := json.Unmarshal(event.Message.Usage, &usageFields); err != nil {
				return anthropicUsage{}, fmt.Errorf("invalid Anthropic start usage")
			}
			phase = readingMessage
		case "message_delta":
			if phase != readingMessage {
				return anthropicUsage{}, fmt.Errorf("unexpected Anthropic message delta")
			}
			updated, err := mergeAnthropicUsage(usage, usageFields, event.Usage)
			if err != nil {
				return anthropicUsage{}, err
			}
			usage = updated
			if event.Delta.StopReason != "" {
				if !validAnthropicStopReason(event.Delta.StopReason) {
					return anthropicUsage{}, fmt.Errorf("invalid Anthropic stop reason")
				}
				phase = completedMessage
			}
		case "message_stop":
			if phase != completedMessage {
				return anthropicUsage{}, fmt.Errorf("incomplete Anthropic message")
			}
			phase = stoppedMessage
		default:
			if event.Type != "ping" && phase != readingMessage {
				return anthropicUsage{}, fmt.Errorf("unexpected Anthropic content event")
			}
		}
	}
	if phase != stoppedMessage {
		return anthropicUsage{}, fmt.Errorf("anthropic: message stop is required")
	}
	return totalAnthropicUsage(usage)
}

// parseAnthropicFrame validates SSE framing and the agreement between the named
// event and JSON payload; a comment-only frame has no event.
type anthropicStreamEvent struct {
	Type    string           `json:"type"`
	Message anthropicMessage `json:"message"`
	Usage   json.RawMessage  `json:"usage"`
	Delta   struct {
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
}

func parseAnthropicFrame(frame []byte) (*anthropicStreamEvent, error) {
	var eventName string
	var data []string
	for line := range bytes.SplitSeq(frame, []byte("\n")) {
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			if eventName != "" {
				return nil, fmt.Errorf("duplicate Anthropic event name")
			}
			eventName = strings.TrimSpace(string(line[6:]))
		case bytes.HasPrefix(line, []byte("data:")):
			data = append(data, strings.TrimPrefix(string(line[5:]), " "))
		case len(line) == 0, bytes.HasPrefix(line, []byte(":")), bytes.HasPrefix(line, []byte("id:")), bytes.HasPrefix(line, []byte("retry:")):
		default:
			return nil, fmt.Errorf("invalid Anthropic event framing")
		}
	}
	if len(data) == 0 {
		if eventName != "" {
			return nil, fmt.Errorf("anthropic: event data is required")
		}
		return nil, nil
	}
	var event anthropicStreamEvent
	if json.Unmarshal([]byte(strings.Join(data, "\n")), &event) != nil || event.Type == "" || event.Type != eventName {
		return nil, fmt.Errorf("invalid Anthropic event stream")
	}
	return &event, nil
}

// mergeAnthropicUsage preserves omitted counters and replaces supplied
// cumulative counts. Nullable optional delta counters mean no update; required
// output and all concrete counts remain strict and nondecreasing.
func mergeAnthropicUsage(previous anthropicUsage, retained map[string]json.RawMessage, raw json.RawMessage) (anthropicUsage, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return anthropicUsage{}, fmt.Errorf("anthropic: delta usage is required")
	}
	output, err := anthropicCount(fields[anthropicOutputTokens], true)
	if err != nil || output < previous.Output {
		return anthropicUsage{}, fmt.Errorf("invalid cumulative Anthropic output usage")
	}
	for key, value := range fields {
		if key != anthropicOutputTokens && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			continue
		}
		retained[key] = value
	}
	merged, err := json.Marshal(retained)
	if err != nil {
		return anthropicUsage{}, fmt.Errorf("invalid Anthropic delta usage")
	}
	updated, err := parseAnthropicUsage(merged)
	if err != nil || updated.Input < previous.Input || updated.CacheRead < previous.CacheRead ||
		updated.CacheWrite5m < previous.CacheWrite5m || updated.CacheWrite1h < previous.CacheWrite1h {
		return anthropicUsage{}, fmt.Errorf("invalid cumulative Anthropic input usage")
	}
	return updated, nil
}
