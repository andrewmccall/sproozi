package model

import (
	"bytes"
	"fmt"
)

// decodeBufferedResponsesStream feeds a whole Responses event stream, split
// into frames, to the stream reader the relay uses, after the reader's
// Content-Type check. It is how the stream tests drive the reader; the
// gateway itself reads a stream only as it relays it.
func decodeBufferedResponsesStream(body []byte, contentType string) (responseUsage, error) {
	if _, err := openAIProtocol.newStream(responsesPath, contentType); err != nil {
		return responseUsage{}, err
	}
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
	result, err := stream.finish()
	if err != nil {
		return result, err
	}
	return normalizeUsage(result, responsesPath)
}

// decodeBufferedAnthropicStream feeds a whole Messages event stream, split
// into frames, to the stream reader the relay uses, after the reader's
// Content-Type check.
func decodeBufferedAnthropicStream(body []byte, contentType string) (anthropicUsage, error) {
	if _, err := anthropicProtocol.newStream(anthropicMessagesPath, contentType); err != nil {
		return anthropicUsage{}, err
	}
	normalized := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	if !bytes.HasSuffix(normalized, []byte("\n\n")) || bytes.Contains(normalized, []byte("\r")) {
		return anthropicUsage{}, fmt.Errorf("truncated Anthropic event stream")
	}
	var stream anthropicStream
	for frame := range bytes.SplitSeq(normalized, []byte("\n\n")) {
		if err := stream.next(frame); err != nil {
			return anthropicUsage{}, err
		}
	}
	return stream.finish()
}
