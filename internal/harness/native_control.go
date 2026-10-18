package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// NativeOp names one fixed operation of the private native supervisor. The
// same names are command arguments to native-session and reply operations.
type NativeOp string

const (
	NativeConfigure NativeOp = "configure"
	NativeActivate  NativeOp = "activate"
	NativeStatus    NativeOp = "status"
	NativeGate      NativeOp = "gate"
	NativeQuiesce   NativeOp = "quiesce"
	NativeAttach    NativeOp = "attach"
)

// NativeState is the supervisor's own lifecycle state. It is a workload
// observation and is independent of any placement's journal phases.
type NativeState string

const (
	NativeUnconfigured NativeState = "unconfigured"
	NativePrepared     NativeState = "prepared"
	NativeActive       NativeState = "active"
	NativeStopping     NativeState = "stopping"
	NativeStopped      NativeState = "stopped"
)

// NativeReply is the private native supervisor reply. It is a workload
// observation, never a physical writer fence or accounting receipt.
type NativeReply struct {
	Op         NativeOp          `json:"op"`
	Error      string            `json:"error,omitempty"`
	State      NativeState       `json:"state,omitempty"`
	PID        int               `json:"pid,omitempty"`
	ThreadID   string            `json:"threadId,omitempty"`
	TurnID     string            `json:"turnId,omitempty"`
	Data       []byte            `json:"data,omitempty"`
	Cursor     uint64            `json:"cursor,omitempty"`
	Gap        bool              `json:"gap,omitempty"`
	Written    int               `json:"written,omitempty"`
	Exit       *NativeExit       `json:"exit,omitempty"`
	Completion *NativeCompletion `json:"completion,omitempty"`
}

// NativeExit is the supervisor's process exit observation, not a container fence.
type NativeExit struct {
	Code   int
	Reason string
}

// NativeCompletion contains bounded Claude observer diagnostics only.
type NativeCompletion struct {
	Started          bool   `json:"started"`
	Completed        bool   `json:"completed"`
	PendingTools     int    `json:"pendingTools"`
	PromptGeneration uint64 `json:"promptGeneration"`
	InputGeneration  uint64 `json:"inputGeneration"`
	LastEvent        string `json:"lastEvent,omitempty"`
	Rejected         string `json:"rejected,omitempty"`
	Observation      string `json:"observation"`
}

func DecodeNativeReply(data []byte, op NativeOp, state NativeState, thread string) (NativeReply, error) {
	reply, err := decodeNativeReply(data, op, state)
	if err != nil {
		return NativeReply{}, err
	}
	if reply.ThreadID != thread {
		return NativeReply{}, errors.New("native thread identity mismatch")
	}
	return reply, nil
}

// DecodeInitialNativeReply allows an initially unbound UUID only until the
// trusted owner has bound a genuine native UUID. Replies cannot replace it.
func DecodeInitialNativeReply(data []byte, op NativeOp, state NativeState, bound string) (NativeReply, error) {
	reply, err := decodeNativeReply(data, op, state)
	if err != nil {
		return NativeReply{}, err
	}
	if reply.ThreadID != "" && !codexThreadID.MatchString(reply.ThreadID) || bound != "" && reply.ThreadID != bound {
		return NativeReply{}, errors.New("initial native thread identity mismatch")
	}
	return reply, nil
}

func decodeNativeReply(data []byte, op NativeOp, state NativeState) (NativeReply, error) {
	var reply NativeReply
	if len(data) == 0 || len(data) > 64<<10 {
		return reply, errors.New("native reply exceeds bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&reply); err != nil {
		return NativeReply{}, err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return NativeReply{}, errors.New("native reply has trailing JSON")
	}
	if reply.Op != op || reply.State != state || reply.Error != "" {
		return NativeReply{}, errors.New("native operation was not acknowledged")
	}
	return reply, nil
}

// NativeControl runs one fixed supervisor operation on the exact admitted
// writer and returns its raw reply. Each placement supplies its own transport.
type NativeControl func(context.Context, NativeOp) ([]byte, error)

// ErrQuiesceUnacknowledged reports that the quiesce request itself failed. The
// native turn may still have completed; only a backend's physical observation
// of the stopped writer can resolve it, and Claude never accepts that shortcut.
var ErrQuiesceUnacknowledged = errors.New("native quiesce reply unavailable")

// QuiesceNative is the shared graceful handoff: gate new input, bind the
// genuine native thread when the launch had none, then wait for the completed
// turn. bind records the observed thread with the trusted owner before quiesce
// reports it. The returned thread is the bound native UUID.
//
// Gateway access stays open throughout so the admitted turn and its tools can
// finish. Revocation, physical fencing and accounting remain backend steps.
func QuiesceNative(ctx context.Context, control NativeControl, thread string, deadline time.Time,
	bind func(context.Context, string) error) (string, error) {
	output, err := control(ctx, NativeGate)
	if err != nil {
		return "", err
	}
	if _, err = DecodeInitialNativeReply(output, NativeGate, NativeStopping, thread); err != nil {
		return "", err
	}
	if thread == "" {
		if thread, err = observeNativeThread(ctx, control, deadline); err != nil {
			return "", err
		}
		if err = bind(ctx, thread); err != nil {
			return "", err
		}
	}
	output, err = control(ctx, NativeQuiesce)
	if err != nil {
		return thread, fmt.Errorf("%w: %w", ErrQuiesceUnacknowledged, err)
	}
	_, err = DecodeNativeReply(output, NativeQuiesce, NativeStopped, thread)
	return thread, err
}

func observeNativeThread(ctx context.Context, control NativeControl, deadline time.Time) (string, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		output, err := control(ctx, NativeStatus)
		if err != nil {
			return "", err
		}
		status, err := DecodeInitialNativeReply(output, NativeStatus, NativeStopping, "")
		if err != nil {
			return "", err
		}
		if status.ThreadID != "" {
			return status.ThreadID, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-ticker.C:
			if !time.Now().Before(deadline) {
				return "", errors.New("native UUID unavailable before deadline")
			}
		}
	}
}
