package gateway

import (
	"context"
	"sync/atomic"

	"github.com/andrewmccall/sproozi/internal/audit"
)

// decision is what one dispatched request has recorded. Tool calls may run on
// other goroutines, so it is atomic.
type decision struct{ recorded atomic.Bool }

type decisionContextKey struct{}

// withDecision starts the request's decision.
func withDecision(ctx context.Context) (context.Context, *decision) {
	d := &decision{}
	return context.WithValue(ctx, decisionContextKey{}, d), d
}

func decisionFrom(ctx context.Context) *decision {
	d, _ := ctx.Value(decisionContextKey{}).(*decision)
	return d
}

// Record writes e through w, the handler's audit writer, and marks the
// request in ctx decided. Every handler records each reply this way. A nil w
// writes nothing and leaves the request undecided.
func Record(ctx context.Context, w audit.Writer, e audit.Event) {
	if w == nil {
		return
	}
	w.Log(e)
	if d := decisionFrom(ctx); d != nil {
		d.recorded.Store(true)
	}
}
