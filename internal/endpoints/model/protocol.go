package model

import (
	"errors"
	"mime"
	"strings"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
)

// providerProtocol is the protocol a Handler speaks with its provider. The
// zero value is OpenAI's, so a Handler built without NewAnthropicHandler
// speaks it.
type providerProtocol uint8

const (
	openAIProtocol providerProtocol = iota
	anthropicProtocol
)

// newStream starts reading a streamed response the provider sent with
// contentType, or reports that the protocol does not stream it. It is the
// only place a stream reader is chosen.
func (p providerProtocol) newStream(path, contentType string) (eventStream, error) {
	switch p {
	case anthropicProtocol:
		var err error
		mediaType, _, parseErr := mime.ParseMediaType(contentType)
		if strings.TrimSpace(contentType) != "" && (parseErr != nil || mediaType != eventStreamMediaType) {
			err = errors.New("unsupported Anthropic streaming response")
		}
		return &anthropicStream{}, err
	case openAIProtocol:
		return &responsesStream{}, checkStreamType(path, contentType)
	}
	return nil, errors.New("unsupported streaming response")
}

// eventStream reads one protocol's streamed response a frame at a time, so
// the gateway can relay each frame as it arrives. It is all of a streamed
// call that differs by protocol: framing, the terminal event, its usage,
// the usage reported before it, and the error event the gateway writes.
// Each protocol keeps its own usage shape: OpenAI's cached tokens are a
// subset of its input, Anthropic's categories are disjoint.
type eventStream interface {
	// next validates one frame, normalized to LF line endings.
	next(frame []byte) error
	// visibleBytes counts the bytes of visible output deltas so far.
	visibleBytes() int64
	// ended reports whether the terminal event has arrived.
	ended() bool
	// completion is the terminal event's usage and the call's end reason,
	// or why the usage is missing or invalid.
	completion(limits api.EndpointBudget) (providerUsage, string, error)
	// reported is the exact usage the provider has reported before its
	// terminal event, and whether it has reported input and output. Only
	// Anthropic's Messages reports it, in the categories the estimate prices.
	reported() (usage anthropicUsage, input, output bool)
	// cutEvent is the error event that ends a stream cut at its reservation.
	cutEvent() string
}

func (s *anthropicStream) visibleBytes() int64 { return s.visible }

func (s *anthropicStream) ended() bool { return s.stopped() }

func (s *anthropicStream) completion(api.EndpointBudget) (providerUsage, string, error) {
	usage, err := s.finish()
	if err != nil {
		return providerUsage{}, "", err
	}
	return providerUsage{usage: anthropicResponseUsage(usage), anthropic: usage}, endCompleted, nil
}

func (s *anthropicStream) reported() (anthropicUsage, bool, bool) {
	return s.usage, s.started(), s.delta
}

func (s *anthropicStream) cutEvent() string {
	return `event: error` + "\n" + `data: {"type":"error","error":{"type":"budget_error","message":"` + budgetCutMessage + `"}}` + "\n\n"
}

func (s *responsesStream) visibleBytes() int64 { return s.visible }

func (s *responsesStream) ended() bool { return s.terminal != "" }

// completion refuses a terminal event without valid usage, and ends a
// response.failed call as a provider error.
func (s *responsesStream) completion(limits api.EndpointBudget) (providerUsage, string, error) {
	usage, err := s.finish()
	if err != nil || invalidTokenUsage(usage, nil, limits) {
		return providerUsage{}, "", errors.New("terminal event omitted required token usage")
	}
	reason := endCompleted
	if s.terminal == responseFailedEvent {
		reason = endUpstreamError
	}
	return providerUsage{usage: usage}, reason, nil
}

func (s *responsesStream) reported() (anthropicUsage, bool, bool) {
	return anthropicUsage{}, false, false
}

func (s *responsesStream) cutEvent() string {
	return `event: error` + "\n" + `data: {"type":"error","code":"budget_exhausted","message":"` + budgetCutMessage + `","param":null}` + "\n\n"
}
