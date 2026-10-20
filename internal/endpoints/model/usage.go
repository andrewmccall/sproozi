package model

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime"
	"strings"
)

// decodeUsage reads provider usage from a whole response body.
func decodeUsage(body []byte, path string, streaming bool, contentType string) (responseUsage, error) {
	var result responseUsage
	if streaming {
		if err := checkStreamType(path, contentType); err != nil {
			return result, err
		}
		var err error
		result, err = decodeStreamUsage(body)
		if err != nil {
			return result, err
		}
	} else if err := json.Unmarshal(body, &result); err != nil {
		return result, err
	}
	return normalizeUsage(result, path)
}

// checkStreamType accepts Responses SSE. The public ChatGPT route can omit
// Content-Type on a valid SSE body. An absent header is not authority to skip
// verification: the same strict framing, terminal-event and usage checks still
// apply. Reject explicit conflicting media types and malformed nonempty headers.
func checkStreamType(path, contentType string) error {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if path != responsesPath || (strings.TrimSpace(contentType) != "" &&
		(err != nil || mediaType != eventStreamMediaType)) {
		return fmt.Errorf("unsupported streaming response")
	}
	return nil
}

// normalizeUsage maps Responses usage onto the chat-completions field names
// and rejects negative or inconsistent counts.
func normalizeUsage(result responseUsage, path string) (responseUsage, error) {
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
	normalized := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	if !bytes.HasSuffix(normalized, []byte("\n\n")) {
		return responseUsage{}, fmt.Errorf("truncated event stream")
	}
	var stream responsesStream
	for frame := range bytes.SplitSeq(normalized, []byte("\n\n")) {
		if err := stream.next(frame); err != nil {
			return responseUsage{}, err
		}
	}
	if stream.terminal == responseFailedEvent {
		return responseUsage{}, fmt.Errorf("unsuccessful event stream")
	}
	return stream.finish()
}

// terminalStatus is the response status each terminal Responses event carries.
var terminalStatus = map[string]string{
	"response.completed": "completed", responseIncompleteEvent: "incomplete", responseFailedEvent: "failed",
}

// responsesStream validates a Responses event stream one frame at a time, so
// the gateway can relay each frame as it arrives. Usage arrives only in the
// terminal event. It counts the bytes of visible output deltas.
type responsesStream struct {
	result   responseUsage
	terminal string
	visible  int64
}

// next validates one frame, normalized to LF line endings.
func (s *responsesStream) next(frame []byte) error {
	var data []string
	for line := range bytes.SplitSeq(frame, []byte("\n")) {
		if bytes.HasPrefix(line, []byte("data:")) {
			data = append(data, strings.TrimPrefix(string(line[5:]), " "))
		}
	}
	if len(data) == 0 {
		return nil
	}
	var event struct {
		Type     string          `json:"type"`
		Response json.RawMessage `json:"response"`
		Delta    json.RawMessage `json:"delta"`
	}
	if s.terminal != "" || json.Unmarshal([]byte(strings.Join(data, "\n")), &event) != nil || event.Type == "" {
		return fmt.Errorf("invalid event stream")
	}
	if event.Type == errorEventType {
		return fmt.Errorf("unsuccessful event stream")
	}
	if want, terminal := terminalStatus[event.Type]; terminal {
		var status struct {
			Status string `json:"status"`
		}
		if json.Unmarshal(event.Response, &status) != nil || status.Status != want ||
			json.Unmarshal(event.Response, &s.result) != nil {
			return fmt.Errorf("invalid completion event")
		}
		s.terminal = event.Type
		return nil
	}
	if strings.HasSuffix(event.Type, ".delta") {
		var delta string
		if json.Unmarshal(event.Delta, &delta) == nil {
			s.visible += int64(len(delta))
		}
	}
	return nil
}

// finish returns the terminal event's usage, normalized.
func (s *responsesStream) finish() (responseUsage, error) {
	if s.terminal == "" {
		return responseUsage{}, fmt.Errorf("completion event missing")
	}
	return normalizeUsage(s.result, responsesPath)
}
