// Package tasks exposes fixed administrator workflows to persistent MCP clients.
// Caller identity remains independent of disposable worker identity.
package tasks

import (
	"context"
	"errors"
	"time"
)

var (
	ErrInvalid     = errors.New("invalid task request")
	ErrDenied      = errors.New("task access denied")
	ErrConflict    = errors.New("request ID conflicts with an existing task")
	ErrExpired     = errors.New("submission expired; use status for an existing task")
	ErrGone        = errors.New("task incarnation is unavailable")
	ErrUnavailable = errors.New("task service unavailable")
)

// Service is the fixed task workflow boundary consumed by the MCP transport.
// Implementations retain exact request identity and independently own authority.
type Service interface {
	Submit(context.Context, Submit) (View, error)
	Status(context.Context, Reference) (View, error)
	Wait(context.Context, Reference, time.Duration) (View, error)
	Cancel(context.Context, Reference) (View, error)
}

// Phase is an observed execution lifecycle, independent of untrusted results.
type Phase string

// Result is bounded workload-controlled output; it never determines success.
type Result struct {
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

// Submit carries untrusted intent, with a replay deadline retained on the Run.
type Submit struct {
	Workflow  string `json:"workflow" jsonschema:"Approved workflow name"`
	RequestID string `json:"requestId" jsonschema:"Stable operation key; reuse this exact key and all input fields after an uncertain response"`
	ExpiresAt string `json:"expiresAt" jsonschema:"UTC RFC3339 replay deadline, at most one hour ahead; store and reuse the exact deadline on retries"`
	Task      string `json:"task" jsonschema:"Untrusted task for the approved worker, at most 16384 characters"`
}

// Reference binds status and cancellation to one incarnation, never just a name.
type Reference struct {
	ID  string `json:"id"`
	UID string `json:"uid"`
}

// View exposes lifecycle and explicitly untrusted answer data, never credentials.
type View struct {
	Reference
	Phase                 Phase   `json:"phase"`
	CancellationRequested bool    `json:"cancellationRequested"`
	StartedAt             string  `json:"startedAt,omitempty"`
	CompletedAt           string  `json:"completedAt,omitempty"`
	Result                *Result `json:"result,omitempty"`
}
