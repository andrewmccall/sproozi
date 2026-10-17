package budget_test

import (
	"context"
	"errors"
	"testing"
	"time"

	api "github.com/andrewmccall/sproozi/api/v1alpha1"
	"github.com/andrewmccall/sproozi/internal/budget"
)

func TestRequestWaitingIsCancellableAndAccountsAreIndependent(t *testing.T) {
	tracker := budget.NewTracker()
	first := tracker.Meter("first/model", api.EndpointBudget{MaxUnits: 100})
	release, err := first.AcquireRequest(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	otherCtx, otherCancel := context.WithTimeout(t.Context(), time.Second)
	defer otherCancel()
	other, err := tracker.Meter("other/model", api.EndpointBudget{MaxUnits: 100}).AcquireRequest(otherCtx)
	if err != nil {
		t.Fatal(err)
	}
	other()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	_, err = tracker.Meter("first/model", api.EndpointBudget{MaxUnits: 100}).AcquireRequest(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued cancellation: %v", err)
	}
	release()
	ctx, cancel = context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	next, err := first.AcquireRequest(ctx)
	if err != nil {
		t.Fatal(err)
	}
	next()
}
