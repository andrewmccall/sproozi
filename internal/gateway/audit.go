package gateway

import (
	"time"

	"github.com/andrewmccall/sproozi/internal/audit"
)

// AuditEvent creates the canonical authenticated identity enrichment shared by
// every capability handler. Callers only supply the operation-specific value.
func (i *RunIdentity) AuditEvent(operation string) audit.Event {
	if i == nil || i.Run == nil || i.Policy == nil {
		return audit.Event{Timestamp: time.Now().UTC(), Operation: operation}
	}
	return audit.Event{
		Timestamp:        time.Now().UTC(),
		RunID:            i.Run.Namespace + "/" + i.Run.Name,
		RunUID:           string(i.Run.UID),
		SAName:           i.SAName,
		SANamespace:      i.SANamespace,
		SAUID:            i.SAUID,
		PolicyName:       i.Policy.Name,
		PolicyGeneration: i.Policy.Generation,
		Operation:        operation,
	}
}
