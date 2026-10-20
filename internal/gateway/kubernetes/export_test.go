package kubernetes

import (
	"context"
	"time"

	"github.com/andrewmccall/sproozi/internal/gateway"
)

// SetClock replaces the clock that times live rechecks.
func (a *Authenticator) SetClock(now func() time.Time) { a.now = now }

// FindCached resolves serviceAccount through the watch cache, as revalidation does.
func (a *Authenticator) FindCached(ctx context.Context, serviceAccount ServiceAccountIdentity) (*gateway.ExecutionGrant, error) {
	return a.find(ctx, a.cached, serviceAccount)
}
