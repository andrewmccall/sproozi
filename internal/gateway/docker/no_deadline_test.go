package docker_test

import (
	"testing"
	"time"
)

// An allocation without a deadline initializes, activates, authenticates
// and seals (ADR 0056).
func TestLocalOwnerAllocationWithoutDeadline(t *testing.T) {
	f := newOwner(t)
	f.launch.Deadline = time.Time{}
	token := f.initialized(t)
	if err := f.owner.Activate(t.Context(), f.launch, f.instance); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Authenticate(t.Context(), token); err != nil {
		t.Fatalf("allocation without deadline refused: %v", err)
	}
	if err := f.owner.Revoke(t.Context(), f.launch, f.instance); err != nil {
		t.Fatal(err)
	}
	if _, err := f.owner.Seal(t.Context(), f.launch, f.instance); err != nil {
		t.Fatal(err)
	}
}
