/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package audit

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// Event is a structured gateway audit record complying with PRD §11.
//
// Forbidden fields (must never appear): credentials, authorization headers,
// prompts, model responses, raw logs, shell output, patch contents.
type Event struct {
	// Timestamp is the UTC time the gateway decision was made.
	Timestamp time.Time `json:"timestamp"`

	// RunID is the AgentRun name (namespace/name) that initiated the request.
	RunID string `json:"runID"`

	// RunUID is the immutable Kubernetes UID of the AgentRun.
	RunUID string `json:"runUID,omitempty"`

	// SAName is the service account name bound to the AgentRun.
	SAName string `json:"saName"`

	// SANamespace and SAUID bind a decision to a full ServiceAccount identity.
	SANamespace string `json:"saNamespace,omitempty"`
	SAUID       string `json:"saUID,omitempty"`

	// PolicyName is the AgentPolicy that governs this run.
	PolicyName string `json:"policyName"`

	// PolicyGeneration is the resource generation of the policy at decision time.
	PolicyGeneration int64 `json:"policyGeneration"`

	// Operation identifies the gateway operation (e.g. "chat.completions").
	Operation string `json:"operation"`

	// These identifiers describe the enforcement guarantee and target.
	Capability       string          `json:"capability,omitempty"`
	SemanticLevel    EnforcementTier `json:"semanticLevel,omitempty"`
	Service          string          `json:"service,omitempty"`
	ProviderAuthMode string          `json:"providerAuthMode,omitempty"`
	Resource         string          `json:"resource,omitempty"`

	// ArtifactURL is a broker-verified artefact identity, never a request URL.
	ArtifactURL string `json:"artifactURL,omitempty"`

	// Allowed reports whether the gateway permitted the request.
	Allowed bool `json:"allowed"`

	// DenyReason is the human-readable explanation when Allowed is false.
	DenyReason string `json:"denyReason,omitempty"`

	// UpstreamStatus is the HTTP status code returned by the upstream model provider.
	UpstreamStatus int `json:"upstreamStatus,omitempty"`

	// TokensUsed is the number of tokens consumed by the upstream response.
	TokensUsed int64 `json:"tokensUsed,omitempty"`

	// TokenBudget is the total token budget configured in the policy.
	TokenBudget int64 `json:"tokenBudget"`

	// TokensRemaining is the remaining token budget after this request.
	TokensRemaining int64 `json:"tokensRemaining"`
}

// WebhookEvent is a structured audit record for webhook ingress decisions.
type WebhookEvent struct {
	Timestamp         time.Time `json:"timestamp"`
	TemplateNamespace string    `json:"templateNamespace"`
	TemplateName      string    `json:"templateName"`
	Operation         string    `json:"operation"`
	Allowed           bool      `json:"allowed"`
	DenyReason        string    `json:"denyReason,omitempty"`
	RunName           string    `json:"runName,omitempty"`
	Source            string    `json:"source,omitempty"`
}

// WebhookWriter is the interface satisfied by Logger for webhook audit events.
type WebhookWriter interface {
	LogWebhook(e WebhookEvent)
}

// Writer is the interface satisfied by Logger; use it in handler configs so
// the gateway handler can be unit-tested with a stub.
type Writer interface {
	Log(e Event)
}

// Logger writes structured JSON audit events as newline-delimited records.
type Logger struct {
	enc *json.Encoder
	mu  sync.Mutex
}

// NewLogger returns a Logger that writes to w.
func NewLogger(w io.Writer) *Logger {
	return &Logger{enc: json.NewEncoder(w)}
}

// Log writes e as a JSON line. Errors are silently discarded intentionally:
// audit logging must never block or disrupt the request path.
func (l *Logger) Log(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.enc.Encode(e)
}

// LogWebhook writes e as a JSON line.
func (l *Logger) LogWebhook(e WebhookEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	_ = l.enc.Encode(e)
}

// EnforcementTier identifies the guarantee of an operation, independently of transport.
type EnforcementTier string

const (
	TierSemantic    EnforcementTier = "semantic"
	TierProtocol    EnforcementTier = "protocol"
	TierDestination EnforcementTier = "destination"
)
