package session

import (
	"context"
	"errors"
)

// ErrUncapturable marks a capture failure that is the fenced visit's own
// native state: the capture helper refused it (checkpoint.ExitStateRefused),
// or what it returned is not a checkpoint of the visit's conversation that
// validates. The writer is fenced, so capturing again finds the same state.
// It is the only capture failure a stop may fall back from (FallsBack).
var ErrUncapturable = errors.New("session: the captured native state does not validate")

// FallsBack reports whether a stop whose capture failed with err keeps
// onCaptureFailure, as StopKeeps returned it, instead of failing. Only a
// failure of the captured state itself does, and only while the stop's
// context has not ended: a capture cut off by the stop's bound or an abort,
// a helper or exec failure and a storage failure all fail the stop, so its
// retry captures again.
func FallsBack(ctx context.Context, err error, onCaptureFailure Kept) bool {
	return onCaptureFailure != 0 && ctx.Err() == nil && errors.Is(err, ErrUncapturable)
}

// Kept is what a stopped visit leaves its session (ADR 0062).
type Kept int

const (
	// KeptCapture is a new checkpoint captured from the stopped visit.
	KeptCapture Kept = iota + 1
	// KeptInput is the checkpoint a resumed visit started from: nothing the
	// visit did is kept.
	KeptInput
	// KeptWorkspace is no checkpoint at all: a first visit leaves the
	// workspace it started from, which the execution retains.
	KeptWorkspace
)

// VisitEnd is what a placement knows, once a visit's writer is fenced, about
// how the visit ended.
type VisitEnd struct {
	// First is a first visit, started from a workspace rather than a
	// checkpoint.
	First bool
	// Turnless is a first visit whose stop reported no conversation (ADR
	// 0032).
	Turnless bool
	// Exited is a harness that ended by itself or at its deadline before the
	// stop's graceful handoff (ADR 0038).
	Exited bool
	// ProvesCompletion is a harness whose own state shows that its last turn
	// completed, so a stopped container may still be captured (ADR 0011).
	ProvesCompletion bool
	// Conversation is a native conversation the visit is known to have: the
	// resumed one, or the one a first visit bound.
	Conversation bool
}

// StopKeeps decides what a stopped visit keeps, the same way for every
// placement. keep is what the stop keeps before any capture. When keep is
// KeptCapture, onCaptureFailure is what the stop keeps instead if the
// captured state does not validate (FallsBack), or zero when every capture
// failure fails the stop, so its retry captures again.
//
// A graceful stop proved its turn complete, so it captures, and a capture
// failure is the stop's failure. A harness that exited proved nothing by
// stopping: its visit is captured only when its own state proves completion
// and it had a conversation, and even then a captured state that does not
// validate keeps what the visit started from rather than failing the stop for
// ever. The capture helper reports such a refusal, such as of a turn the
// harness left unfinished, with its own exit code, which each placement maps
// onto ErrUncapturable.
func StopKeeps(e VisitEnd) (keep, onCaptureFailure Kept) {
	start := KeptInput
	if e.First {
		start = KeptWorkspace
	}
	switch {
	case e.Turnless:
		return start, 0
	case e.Exited && (!e.ProvesCompletion || !e.Conversation):
		return start, 0
	case e.Exited:
		return KeptCapture, start
	}
	return KeptCapture, 0
}
