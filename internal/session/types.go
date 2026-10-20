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
// A first visit started from a Git workspace has neither ThreadID nor
// CheckpointDigest: the placement observes the genuine native thread during the
// visit and reports it in Stopped.ThreadID. No layer rewrites a Launch.
type Launch struct {
	Harness harness.NativeKind `json:"harness"`
	// ExecutionID is the public executionId. Its wire name predates the split
	// between durable sessions and their executions.
	ExecutionID       string               `json:"sessionId"`
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
	// ProfileDigest names the user's profile archive for this visit. Docker
	// mounts the live folder instead; remote placements store the archive.
	ProfileDigest string `json:"profileDigest,omitempty"`
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
// Launch and Receipt.Launch are the exact launch the visit started from.
// ThreadID is the genuine native thread a first visit bound; a placement may
// omit it when it equals Launch.ThreadID. It is empty for a first visit only
// when the visit was cancelled before its thread was observed.
type Stopped struct {
	Launch           Launch      `json:"launch"`
	Incarnation      Incarnation `json:"incarnation"`
	ActualStopped    bool        `json:"actualStopped"`
	ThreadID         string      `json:"threadId,omitempty"`
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
	// Prepare restores the input inertly: the checkpoint named by
	// Launch.CheckpointDigest, or a captured Git workspace when it is empty.
	Prepare(context.Context, Launch, []byte) (Incarnation, error)
	Activate(context.Context, Launch, Incarnation) error
	Stop(context.Context, Launch, Incarnation) (Stopped, error)
	Inspect(context.Context, Launch) (Observation, error)
	// Abort revokes first and forcibly stops; a fresh checkpoint is optional.
	Abort(context.Context, Launch, Incarnation) (Observation, error)
}

// PreparationAborter is implemented by placements that can fence a preparation
// which never reported an incarnation, without inspecting or resuming it.
// Cancellation uses it in place of Inspect for such a pending visit.
type PreparationAborter interface {
	AbortPreparation(context.Context, Launch) error
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

// Record is one execution: a bounded period of work with one deadline and
// allowance, from its first visit through every move. It is a read-only caller
// snapshot; mutations go through Coordinator.
//
// The first operation starts the first visit. Its input is the committed
// Checkpoint when the execution resumes a conversation, or the retained Git
// Workspace for a new one; ThreadID and Checkpoint are then empty until the
// first stop binds the genuine thread. Session names the durable session that
// owns the execution; it is empty when the execution is the session itself.
// PendingOperation names the one unfinished operation, always the last one.
type Record struct {
	Harness harness.NativeKind `json:"harness"`
	Version int                `json:"version"`
	// ID is the public executionId.
	ID                string               `json:"id"`
	Session           string               `json:"session,omitempty"`
	ThreadID          string               `json:"threadId,omitempty"`
	Workspace         string               `json:"workspace,omitempty"`
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
