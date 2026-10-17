// Package session owns the trusted home journal and sequential placement escrow.
// Native checkpoints are continuity data; placement observations and sealed
// receipts independently establish stopped writers and consumed authority.
package session

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"

	"github.com/andrewmccall/sproozi/internal/budget"
	"github.com/andrewmccall/sproozi/internal/checkpoint"
)

// TerminalPlacement attaches the fixed native terminal protocol to an exact
// admitted incarnation. Closing an attachment does not stop its native writer.
type TerminalPlacement interface {
	Attach(context.Context, Launch, Incarnation, io.Reader, io.Writer) error
}

// Limit distinguishes an exhausted bounded allowance from an unbounded one.
type Limit struct {
	Bounded bool  `json:"bounded"`
	Value   int64 `json:"value"`
}

// Allowance carries independently bounded token/request units and dollar micros.
type Allowance struct {
	Units      Limit `json:"units"`
	CostMicros Limit `json:"costMicros"`
}

// Launch is immutable trusted placement intent. Checkpoint contents grant none of it.
type Launch struct {
	Harness           harness.NativeKind   `json:"harness"`
	SessionID         string               `json:"sessionId"`
	OperationID       string               `json:"operationId"`
	AllocationID      string               `json:"allocationId"`
	Epoch             uint64               `json:"epoch"`
	Placement         string               `json:"placement"`
	ThreadID          string               `json:"threadId"`
	Task              string               `json:"task,omitempty"`
	InstructionDigest string               `json:"instructionDigest"`
	Deadline          time.Time            `json:"deadline"`
	Allowances        map[string]Allowance `json:"allowances"`
	CheckpointDigest  string               `json:"checkpointDigest"`
	IncludeIgnored    []string             `json:"includeIgnored,omitempty"`
}

// Incarnation identifies exact resources, rather than a reusable execution name.
type Incarnation struct {
	ExecutionID string `json:"executionId"`
	ResourceUID string `json:"resourceUid"`
}

// Receipt comes directly from the trusted placement after atomic accounting seal.
// Usage includes outstanding reservations and covers every granted capability.
type Receipt struct {
	Launch      Launch                         `json:"launch"`
	Incarnation Incarnation                    `json:"incarnation"`
	Usage       map[string]budget.TrustedUsage `json:"usage"`
}

// Stopped separates the independently observed writer fence from accounting.
// Nil Receipt means usage is unknown and all allowance stays encumbered.
type Stopped struct {
	Launch           Launch      `json:"launch"`
	Incarnation      Incarnation `json:"incarnation"`
	ActualStopped    bool        `json:"actualStopped"`
	Receipt          *Receipt    `json:"receipt,omitempty"`
	Checkpoint       []byte      `json:"-"`
	CheckpointDigest string      `json:"checkpointDigest"`
}

// PlacementState distinguishes an absent keyed operation from an unreachable one.
type PlacementState string

const (
	PlacementAbsent PlacementState = "absent"
	// Preparing is retained incomplete intent, with an exact incarnation only
	// once observed. It grants neither successful restore nor activation.
	PlacementPreparing PlacementState = "preparing"
	PlacementPrepared  PlacementState = "prepared"
	PlacementActive    PlacementState = "active"
	PlacementStopped   PlacementState = "stopped"
	PlacementUnknown   PlacementState = "unknown"
)

// Observation is returned for the retained operation/allocation, never just a name.
type Observation struct {
	Launch      Launch
	Incarnation Incarnation
	State       PlacementState
	Stopped     *Stopped
}

// ErrUsageUnsealed reports that a placement proved the exact writer stopped but
// could not seal its usage. The allocation stays encumbered; retrying Stop
// repeats only the seal and capture, never the native handoff.
var ErrUsageUnsealed = errors.New("session: writer stopped but usage is not sealed")

// Placement must retain exact intent on retries. Prepare is inert, Activate is
// idempotent for its incarnation, and Stop retains an immutable sealed receipt.
// Inspect errors/unknown observations cannot be converted into absence or zero.
//
// Stop has exactly three outcomes:
//   - nil error: the writer is fenced, Receipt is sealed and Checkpoint is captured.
//   - an error wrapping ErrUsageUnsealed: Stopped reports ActualStopped with a
//     nil Receipt. The writer is fenced; only accounting is unknown.
//   - any other error: whether the writer stopped is unknown.
//
// Abort reports a fenced writer as PlacementStopped; a nil Stopped.Receipt
// there likewise means the writer stopped but its usage is not sealed.
type Placement interface {
	Prepare(context.Context, Launch, []byte) (Incarnation, error)
	Activate(context.Context, Launch, Incarnation) error
	Stop(context.Context, Launch, Incarnation) (Stopped, error)
	Inspect(context.Context, Launch) (Observation, error)
	// Abort revokes first and forcibly stops; a fresh checkpoint is optional.
	Abort(context.Context, Launch, Incarnation) (Observation, error)
}

// Start imports the first genuinely completed checkpoint with its trusted source
// stop/usage receipt. Its session/native UUIDs were generated by that real source.
// Source.Allowances are the approved original ceilings, not artifact metadata.
type Start struct {
	Source      Launch
	Incarnation Incarnation
	Stopped     Stopped
}

// Checkpoint identifies a committed private file within the coordinator directory.
type Checkpoint struct {
	Metadata checkpoint.Metadata `json:"metadata"`
	Path     string              `json:"path"`
}

// Visit keeps immutable intent and receipts after the allocation is released.
type Visit struct {
	CheckpointDigest string      `json:"checkpointDigest"`
	ActualStopped    bool        `json:"actualStopped"`
	Launch           Launch      `json:"launch"`
	Incarnation      Incarnation `json:"incarnation"`
	Receipt          *Receipt    `json:"receipt,omitempty"`
}
// Phase summarises the execution for callers. Every save checks it against the
// structure it describes (see validatePhase), so a phase never contradicts the
// active visit, the pending operation or the cancellation intent.
type Phase string

const (
	// PhaseIdle has no writer and nothing in progress.
	PhaseIdle Phase = "idle"
	// PhasePreparing and PhaseActivating have a pending operation whose
	// destination is not yet active, and no other writer.
	PhasePreparing  Phase = "preparing"
	PhaseActivating Phase = "activating"
	// PhaseActive has one admitted writer and nothing in progress.
	PhaseActive Phase = "active"
	// PhaseStopping is a graceful stop of the active writer in progress.
	PhaseStopping Phase = "stopping"
	// PhaseUnknownStop is a stop or cancellation that could not prove the
	// writer stopped. The allocation stays encumbered.
	PhaseUnknownStop Phase = "unknown-stop"
	// PhaseUnknownUsage has a fenced writer whose usage is not sealed.
	PhaseUnknownUsage Phase = "unknown-usage"
	// PhaseCancelling has durable cancellation intent not yet carried out.
	PhaseCancelling Phase = "cancelling"
	// PhaseCancelled has carried out cancellation; nothing can start again.
	PhaseCancelled Phase = "cancelled"
)

// Stage is the next step of an operation. Done stages are terminal.
type Stage string

const (
	stagePrepare   Stage = "prepare"
	stageActivate  Stage = "activate"
	stageStop      Stage = "stop"
	stageComplete  Stage = "complete"
	stageCancelled Stage = "cancelled"
)

// Operation is the durable move intent. Recovery uses its same allocation and epoch.
type Operation struct {
	ID          string       `json:"id"`
	Destination string       `json:"destination"`
	Finalize    bool         `json:"finalize,omitempty"`
	Launch      *Launch      `json:"launch,omitempty"`
	Incarnation *Incarnation `json:"incarnation,omitempty"`
	Stage       Stage        `json:"stage"`
}

func (o Operation) done() bool { return o.Stage == stageComplete || o.Stage == stageCancelled }

// Record is a read-only caller snapshot; mutations go through Coordinator.
// PendingOperation names the one unfinished operation, always the last one.
type Record struct {
	Harness           harness.NativeKind   `json:"harness"`
	Version           int                  `json:"version"`
	ID                string               `json:"id"`
	ThreadID          string               `json:"threadId"`
	InstructionDigest string               `json:"instructionDigest"`
	IncludeIgnored    []string             `json:"includeIgnored,omitempty"`
	Deadline          time.Time            `json:"deadline"`
	Ceiling           map[string]Allowance `json:"ceiling"`
	Remaining         map[string]Allowance `json:"remaining"`
	Checkpoint        Checkpoint           `json:"checkpoint"`
	Phase             Phase                `json:"phase"`
	Epoch             uint64               `json:"epoch"`
	Active            *Visit               `json:"active,omitempty"`
	Visits            []Visit              `json:"visits"`
	Operations        []Operation          `json:"operations"`
	PendingOperation  string               `json:"pendingOperation,omitempty"`
	CancelOperation   string               `json:"cancelOperation,omitempty"`
}
