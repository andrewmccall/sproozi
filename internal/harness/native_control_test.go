package harness

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestNativeReplyBindsOperationStateAndThread(t *testing.T) {
	const thread = "12345678-1234-1234-1234-123456789abc"
	const valid = `{"op":"activate","state":"active","threadId":"12345678-1234-1234-1234-123456789abc","pid":42}`
	reply, err := DecodeNativeReply([]byte(valid), "activate", "active", thread)
	if err != nil || reply.PID != 42 || reply.ThreadID != thread {
		t.Fatalf("reply=%+v error=%v", reply, err)
	}
	for _, data := range []string{
		strings.Replace(valid, `"activate"`, `"status"`, 1),
		strings.Replace(valid, `"active"`, `"prepared"`, 1),
		strings.Replace(valid, thread, "22345678-1234-1234-1234-123456789abc", 1),
		strings.Replace(valid, `"pid":42`, `"error":"failed"`, 1),
		strings.Replace(valid, `"pid":42`, `"unknown":42`, 1),
		valid + `{}`, `null`, strings.Repeat(" ", 64<<10) + valid,
	} {
		if _, err := DecodeNativeReply([]byte(data), "activate", "active", thread); err == nil {
			t.Fatalf("invalid reply accepted: %.200s", data)
		}
	}
}

func TestInitialNativeReplyBindsObservedIdentityOnce(t *testing.T) {
	const thread = "12345678-1234-1234-1234-123456789abc"
	const empty = `{"op":"status","state":"active"}`
	const observed = `{"op":"status","state":"active","threadId":"12345678-1234-1234-1234-123456789abc"}`
	for _, tc := range []struct {
		data, bound, want string
		ok                bool
	}{
		{empty, "", "", true}, {observed, "", thread, true}, {observed, thread, thread, true},
		{empty, thread, "", false}, {observed, "22345678-1234-1234-1234-123456789abc", "", false},
		{strings.Replace(observed, thread, "not-a-native-id", 1), "", "", false},
	} {
		reply, err := DecodeInitialNativeReply([]byte(tc.data), "status", "active", tc.bound)
		if (err == nil) != tc.ok || err == nil && reply.ThreadID != tc.want {
			t.Fatalf("bound=%q reply=%+v error=%v", tc.bound, reply, err)
		}
	}
}

func TestQuiesceNativeBindsObservedThreadBeforeCompletedTurn(t *testing.T) {
	const thread = "12345678-1234-1234-1234-123456789abc"
	var calls []NativeOp
	var bound string
	statusPolls := 0
	control := func(_ context.Context, op NativeOp) ([]byte, error) {
		calls = append(calls, op)
		switch op {
		case NativeGate:
			return []byte(`{"op":"gate","state":"stopping"}`), nil
		case NativeStatus:
			statusPolls++
			if statusPolls == 1 {
				return []byte(`{"op":"status","state":"stopping"}`), nil
			}
			return []byte(`{"op":"status","state":"stopping","threadId":"` + thread + `"}`), nil
		case NativeQuiesce:
			if bound == "" {
				return nil, errors.New("quiesce before binding")
			}
			return []byte(`{"op":"quiesce","state":"stopped","threadId":"` + thread + `"}`), nil
		}
		return nil, errors.New("unexpected operation")
	}
	bind := func(_ context.Context, observed string) error { bound = observed; return nil }
	got, err := QuiesceNative(t.Context(), control, "", time.Now().Add(time.Minute), bind)
	if err != nil || got != thread || bound != thread {
		t.Fatalf("thread=%q bound=%q error=%v", got, bound, err)
	}
	want := []NativeOp{NativeGate, NativeStatus, NativeStatus, NativeQuiesce}
	if !slices.Equal(calls, want) {
		t.Fatalf("operations = %v, want %v", calls, want)
	}
}

func TestQuiesceNativeSeparatesLostReplyFromRejectedTurn(t *testing.T) {
	const thread = "12345678-1234-1234-1234-123456789abc"
	gate := []byte(`{"op":"gate","state":"stopping","threadId":"` + thread + `"}`)
	noBind := func(context.Context, string) error { return errors.New("a bound thread must not be rebound") }
	lost := func(_ context.Context, op NativeOp) ([]byte, error) {
		if op == NativeGate {
			return gate, nil
		}
		return nil, errors.New("connection reset")
	}
	if _, err := QuiesceNative(t.Context(), lost, thread, time.Now().Add(time.Minute), noBind); !errors.Is(err, ErrQuiesceUnacknowledged) {
		t.Fatalf("lost quiesce reply: %v", err)
	}
	rejected := func(_ context.Context, op NativeOp) ([]byte, error) {
		if op == NativeGate {
			return gate, nil
		}
		return []byte(`{"op":"quiesce","state":"stopping","error":"no completed turn"}`), nil
	}
	_, err := QuiesceNative(t.Context(), rejected, thread, time.Now().Add(time.Minute), noBind)
	if err == nil || errors.Is(err, ErrQuiesceUnacknowledged) {
		t.Fatalf("a rejected quiesce must not look like a lost reply: %v", err)
	}
}

// A first visit reported turnless stops without binding any thread, and only
// a turnless reply may end quiesce with no thread.
func TestQuiesceNativeStopsTurnlessFirstVisitWithoutBinding(t *testing.T) {
	quiesce := `{"op":"quiesce","state":"stopped","turnless":true}`
	control := func(_ context.Context, op NativeOp) ([]byte, error) {
		switch op {
		case NativeGate:
			return []byte(`{"op":"gate","state":"stopping"}`), nil
		case NativeStatus:
			return []byte(`{"op":"status","state":"stopping","turnless":true}`), nil
		case NativeQuiesce:
			return []byte(quiesce), nil
		}
		return nil, errors.New("unexpected operation")
	}
	bind := func(context.Context, string) error { return errors.New("turnless visit bound a thread") }
	got, err := QuiesceNative(t.Context(), control, "", time.Now().Add(time.Minute), bind)
	if err != nil || got != "" {
		t.Fatalf("thread=%q error=%v", got, err)
	}
	quiesce = `{"op":"quiesce","state":"stopped"}`
	if _, err = QuiesceNative(t.Context(), control, "", time.Now().Add(time.Minute), bind); err == nil {
		t.Fatal("quiesce without a thread or turnless report accepted")
	}
}

// A quiesce reply that is still waiting becomes ErrNativeTurnPending naming
// what it waits for, not an unknown stop.
func TestQuiesceNativeReportsPendingTurn(t *testing.T) {
	const thread = "12345678-1234-1234-1234-123456789abc"
	control := func(_ context.Context, op NativeOp) ([]byte, error) {
		switch op {
		case NativeGate:
			return []byte(`{"op":"gate","state":"stopping","threadId":"` + thread + `"}`), nil
		case NativeQuiesce:
			return []byte(`{"op":"quiesce","state":"stopping","waiting":"the current Codex turn to complete"}`), nil
		}
		return nil, errors.New("unexpected operation")
	}
	_, err := QuiesceNative(t.Context(), control, thread, time.Now().Add(time.Minute), nil)
	if !errors.Is(err, ErrNativeTurnPending) || !strings.Contains(err.Error(), "current Codex turn") {
		t.Fatalf("pending quiesce: %v", err)
	}
}
