package harness

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// NativeReply is the private native supervisor reply. It is a workload
// observation, never a physical writer fence or accounting receipt.
type NativeReply struct {
	Op         string            `json:"op"`
	Error      string            `json:"error,omitempty"`
	State      string            `json:"state,omitempty"`
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

func DecodeNativeReply(data []byte, op, state, thread string) (NativeReply, error) {
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
func DecodeInitialNativeReply(data []byte, op, state, bound string) (NativeReply, error) {
	reply, err := decodeNativeReply(data, op, state)
	if err != nil {
		return NativeReply{}, err
	}
	if reply.ThreadID != "" && !codexThreadID.MatchString(reply.ThreadID) || bound != "" && reply.ThreadID != bound {
		return NativeReply{}, errors.New("initial native thread identity mismatch")
	}
	return reply, nil
}

func decodeNativeReply(data []byte, op, state string) (NativeReply, error) {
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
