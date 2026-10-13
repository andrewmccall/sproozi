package gateway

import (
	"time"

	"github.com/andrewmccall/sproozi/internal/audit"
)

// AuditEvent creates the canonical authenticated identity enrichment shared by
// every capability handler. Callers only supply the operation-specific value.
func (i *ExecutionGrant) AuditEvent(operation string) audit.Event {
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
