package gateway

import (
	"context"
	"time"

	"github.com/andrewmccall/sproozi/internal/audit"
)

// AuditEvent creates the canonical audit record shared by every capability
// handler: the authenticated identity plus the dispatching route's capability
// and tier. Callers only supply the operation-specific value.
func AuditEvent(ctx context.Context, operation string) audit.Event {
	identity, _ := IdentityFromContext(ctx)
	event := identity.auditEvent(operation)
	if route, ok := ctx.Value(routeContextKey{}).(Route); ok {
		event.Capability, event.SemanticLevel = route.Capability, route.Tier
	}
	return event.WithContext(ctx)
}

func (i *ExecutionGrant) auditEvent(operation string) audit.Event {
	if i == nil || i.Execution.UID == "" {
		return audit.Event{Timestamp: time.Now().UTC(), Operation: operation}
	}
	event := audit.Event{
		Timestamp:        time.Now().UTC(),
		ExecutionBackend: i.Execution.Backend,
		RunID:            i.Execution.Namespace + "/" + i.Execution.Name,
		RunUID:           i.Execution.UID,
		PolicyName:       i.Policy.Name,
		PolicyGeneration: i.Policy.Generation,
		Operation:        operation,
	}
	if i.Execution.Namespace == "" {
		event.RunID = i.Execution.Name
	}
	if i.Authority.Backend == BackendKubernetes {
		event.SAName, event.SANamespace, event.SAUID = i.Authority.Name, i.Authority.Namespace, i.Authority.UID
	}
	return event
}
