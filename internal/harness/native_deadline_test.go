package harness

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"testing"
	"time"
)

// A first visit without a deadline waits for its thread like any other: it
// is not reported as past its deadline at once (ADR 0056).
func TestQuiesceNativeWithoutDeadline(t *testing.T) {
	const thread = "12345678-1234-1234-1234-123456789abc"
	polls := 0
	control := func(_ context.Context, op NativeOp) ([]byte, error) {
		switch op {
		case NativeGate:
			return []byte(`{"op":"gate","state":"stopping"}`), nil
		case NativeStatus:
			if polls++; polls < 3 {
				return []byte(`{"op":"status","state":"stopping"}`), nil
			}
			return []byte(`{"op":"status","state":"stopping","threadId":"` + thread + `"}`), nil
		case NativeQuiesce:
			return []byte(`{"op":"quiesce","state":"stopped","threadId":"` + thread + `"}`), nil
		}
		return nil, errors.New("unexpected operation")
	}
	bind := func(context.Context, string) error { return nil }
	got, err := QuiesceNative(t.Context(), control, "", time.Time{}, bind)
	if err != nil || got != thread {
		t.Fatalf("thread=%q error=%v", got, err)
	}
}

// A launch contract without a deadline is valid; one past its deadline is not.
func TestNativeLaunchValidateWithoutDeadline(t *testing.T) {
	c := NativeLaunch{Harness: NativeCodex, Env: []string{}, RolloutMaxBytes: 1, Instructions: "i",
		ClientConfig:      &NativeClientConfig{MCPGateway: "https://gateway.test"},
		InstructionDigest: fmt.Sprintf("%x", sha256.Sum256([]byte("i")))}
	if err := c.Validate(time.Now()); err != nil {
		t.Fatalf("launch without deadline refused: %v", err)
	}
	c.Deadline = time.Now().Add(-time.Second)
	if err := c.Validate(time.Now()); err == nil {
		t.Fatal("expired launch accepted")
	}
}
