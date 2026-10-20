package session

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"

	"github.com/andrewmccall/sproozi/internal/telemetry"
)

// observe opens a coordinator span in the execution's trace. The returned
// function ends it with the step's final error.
func observe(ctx context.Context, r Record, name string, attrs ...attribute.KeyValue) (context.Context, func(*error)) {
	attrs = append(attrs, attribute.String(telemetry.ExecutionID, r.ID), attribute.String(telemetry.SessionID, cmpSession(r)))
	ctx, span := telemetry.Tracer().Start(telemetry.WithinExecution(ctx, r.ID), name)
	span.SetAttributes(attrs...)
	return ctx, func(err *error) {
		if *err != nil {
			// Coordinator errors name states and stages, never credentials or content.
			span.SetStatus(codes.Error, (*err).Error())
		}
		span.End()
	}
}

// observeVisit opens the span of one stage of one visit.
func observeVisit(ctx context.Context, r Record, name string, l Launch) (context.Context, func(*error)) {
	return observe(ctx, r, name, attribute.String(telemetry.VisitID, l.AllocationID),
		attribute.String(telemetry.Placement, l.Placement), attribute.Int64("sproozi.visit.epoch", int64(l.Epoch)))
}

func operationName(r Record, index int) string {
	switch {
	case r.Operations[index].Finalize:
		return "finalize execution"
	case index == 0:
		return "start execution"
	default:
		return "move execution"
	}
}

// cmpSession names the durable session, which is the execution itself when
// Record.Session is empty.
func cmpSession(r Record) string {
	if r.Session != "" {
		return r.Session
	}
	return r.ID
}
