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
	"unicode/utf8"
)

// Event is a structured gateway audit record complying with PRD §11.
//
// Forbidden fields (must never appear): credentials, authorization headers,
// prompts, model responses, raw logs, shell output, patch contents.
type Event struct {
	// Timestamp is the UTC time the gateway decision was made.
	Timestamp time.Time `json:"timestamp"`

	// ExecutionBackend identifies the authority owner, such as kubernetes or docker.
	ExecutionBackend string `json:"executionBackend,omitempty"`

	// RunID retains the original wire name for the execution's namespace/name.
	// Kubernetes executions use an AgentRun; Docker executions have no CRD.
	RunID string `json:"runID"`

	// RunUID retains the original wire name for the immutable execution ID.
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

	// Model is the model a provider accepted. It is recorded only after the
	// provider succeeds, so it is never unvalidated workload input.
	Model string `json:"model,omitempty"`

	// UpstreamStatus is the HTTP status code returned by the upstream model provider.
	UpstreamStatus int `json:"upstreamStatus,omitempty"`

	// TokensUsed is the number of tokens consumed by the upstream response.
	TokensUsed int64 `json:"tokensUsed,omitempty"`

	// InputTokens and OutputTokens split TokensUsed when the provider reports both.
	InputTokens  int64 `json:"inputTokens,omitempty"`
	OutputTokens int64 `json:"outputTokens,omitempty"`

	// UsageSource says how a model call was charged: provider (the provider's
	// reported usage), estimated (an estimate of an interrupted call) or
	// reservation (the call's whole reservation).
	UsageSource string `json:"usageSource,omitempty"`

	// EndReason says how a model call ended: completed, client_cancelled,
	// deadline, upstream_error, budget_cut, parse_limit or exceeded.
	EndReason string `json:"endReason,omitempty"`

	// TokenBudget is the total token budget configured in the policy.
	TokenBudget int64 `json:"tokenBudget"`

	// TokensRemaining is the observed remaining budget; nil means unavailable.
	TokensRemaining *int64 `json:"tokensRemaining"`
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
	e = Bound(e)
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

// Bound cuts the free-text fields that can carry workload-chosen text, such as
// a requested authority or path on a denial, so a sandbox cannot bloat the
// audit stream or anything that retains events.
func Bound(e Event) Event {
	e.Operation = bounded(e.Operation)
	e.Resource = bounded(e.Resource)
	e.DenyReason = bounded(e.DenyReason)
	e.ArtifactURL = bounded(e.ArtifactURL)
	return e
}

// maxFieldBytes bounds free-text audit fields that may hold workload input.
const maxFieldBytes = 512

// bounded cuts s to at most maxFieldBytes on a rune boundary and marks the cut.
func bounded(s string) string {
	if len(s) <= maxFieldBytes {
		return s
	}
	cut := maxFieldBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}
