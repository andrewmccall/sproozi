package model

// providerProtocol is the protocol a Handler speaks with its provider. The
// zero value is OpenAI's, so a Handler built without NewAnthropicHandler
// speaks it.
type providerProtocol uint8

const (
	openAIProtocol providerProtocol = iota
	anthropicProtocol
)
