package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"strings"
)

// decodeUsage reads provider usage without forwarding partial model output.
// Responses SSE is supported as a bounded batch of events, not a live relay.
func decodeUsage(body []byte, path string, streaming bool, contentType string) (responseUsage, error) {
	var result responseUsage
	if streaming {
		mediaType, _, err := mime.ParseMediaType(contentType)
		// The public ChatGPT route can omit Content-Type on a valid SSE body.
		// An absent header is not authority to skip verification: the same strict
		// framing, terminal-completion and usage checks below still apply. Reject
		// explicit conflicting media types and malformed nonempty headers.
		if path != responsesPath || (strings.TrimSpace(contentType) != "" &&
			(err != nil || mediaType != "text/event-stream")) {
			return result, fmt.Errorf("unsupported streaming response")
		}
		result, err = decodeStreamUsage(body)
		if err != nil {
			return result, err
		}
	} else if err := json.Unmarshal(body, &result); err != nil {
		return result, err
	}
	if path == responsesPath && result.Usage != nil {
		result.Usage.PromptTokens, result.Usage.CompletionTokens = result.Usage.InputTokens, result.Usage.OutputTokens
		if result.Usage.InputTokensDetails != nil {
			result.Usage.PromptTokensDetails = result.Usage.InputTokensDetails
		}
	}
	if usage := result.Usage; usage != nil {
		for _, count := range []*int64{usage.TotalTokens, usage.PromptTokens, usage.CompletionTokens} {
			if count != nil && *count < 0 {
				return result, fmt.Errorf("negative token usage")
			}
		}
		if usage.PromptTokensDetails != nil && usage.PromptTokensDetails.CachedTokens != nil {
			cached := *usage.PromptTokensDetails.CachedTokens
			if cached < 0 || (usage.PromptTokens != nil && cached > *usage.PromptTokens) {
				return result, fmt.Errorf("invalid cached token usage")
			}
		}
	}
	return result, nil
}

func decodeStreamUsage(body []byte) (responseUsage, error) {
	var result responseUsage
	normalized := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	if !bytes.HasSuffix(normalized, []byte("\n\n")) {
		return result, fmt.Errorf("truncated event stream")
	}
	completed := false
	for frame := range bytes.SplitSeq(normalized, []byte("\n\n")) {
		var data []string
		for line := range bytes.SplitSeq(frame, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				data = append(data, strings.TrimPrefix(string(line[5:]), " "))
			}
		}
		if len(data) == 0 {
			continue
		}
		var event struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
		}
		if completed || json.Unmarshal([]byte(strings.Join(data, "\n")), &event) != nil || event.Type == "" {
			return result, fmt.Errorf("invalid event stream")
		}
		if event.Type == errorEventType || event.Type == responseFailedEvent || event.Type == responseIncompleteEvent {
			return result, fmt.Errorf("unsuccessful event stream")
		}
		if event.Type == "response.completed" {
			var status struct {
				Status string `json:"status"`
			}
			if json.Unmarshal(event.Response, &status) != nil || status.Status != "completed" ||
				json.Unmarshal(event.Response, &result) != nil {
				return result, fmt.Errorf("invalid completion event")
			}
			completed = true
		}
	}
	if !completed {
		return result, fmt.Errorf("completion event missing")
	}
	return result, nil
}
