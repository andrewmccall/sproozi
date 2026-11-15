package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/work"
	"github.com/andrewmccall/sproozi/internal/workspace"
	"github.com/google/uuid"
	"golang.org/x/term"
)

// This file runs interactive sessions a cluster's remote executor owns: the
// executor starts, stops, resumes and attaches them; this machine keeps
// nothing (ADR 0057).

// remoteFlag selects the default cluster.
const remoteFlag = "--remote"

// Phases of a cluster's session, as the coordinator names them.
var (
	phaseRunning      = string(session.PhaseActive)
	phaseStarting     = string(session.PhasePreparing)
	phaseActivating   = string(session.PhaseActivating)
	phaseStopping     = string(session.PhaseStopping)
	phaseUnknownStop  = string(session.PhaseUnknownStop)
	phaseUnknownUsage = string(session.PhaseUnknownUsage)
	phaseCancelling   = string(session.PhaseCancelling)
)

// remoteStartPoll is how often attach checks whether a starting session runs.
var remoteStartPoll = 2 * time.Second

// remoteStopRetry is how long stop pauses before asking again while the
// agent's current turn runs.
var remoteStopRetry = 2 * time.Second

// remoteFlags are the flags that reach this command's cluster again, for the
// commands output suggests: --context and --namespace as given, otherwise
// --remote.
func (q *options) remoteFlags() string {
	var flags []string
	if q.context != "" {
		flags = append(flags, "--context", shellQuote(q.context))
	}
	if q.namespace != "" {
		flags = append(flags, "--namespace", shellQuote(q.namespace))
	}
	if len(flags) == 0 {
		return remoteFlag
	}
	return strings.Join(flags, " ")
}

// remoteCommand is a command for session id in this command's cluster.
func (q *options) remoteCommand(name, id string) string {
	return fmt.Sprintf("sproozi %s %s %s", name, shortID(id), q.remoteFlags())
}

// pullCommand brings session id from this command's cluster; pull takes no
// --remote, since it always reaches a cluster.
func (q *options) pullCommand(id string) string {
	command := "sproozi pull " + shortID(id)
	if flags := q.remoteFlags(); flags != remoteFlag {
		command += " " + flags
	}
	return command
}

// captureUpload copies the Git repository at path as a local run does, within
// the executor's checkpoint limit, for an interactive session to start from.
func captureUpload(ctx context.Context, path string, includeIgnored []string, limit int64) ([]byte, error) {
	if limit < 1 {
		return nil, errors.New("the cluster's remote executor did not report its checkpoint limit, so nothing " +
			"was uploaded; update the executor, or leave out PATH to start from the approved repository")
	}
	data, err := workspace.CaptureLocal(ctx, path, includeIgnored, limit)
	switch {
	case err == nil:
		return data, nil
	case ctx.Err() != nil:
		return nil, err
	case errors.Is(err, workspace.ErrByteLimit):
		return nil, fmt.Errorf("%w; the cluster accepts uploads of at most %.1f MiB (its maxCheckpointBytes): "+
			"ask its administrator to raise it, or leave out PATH to start from the approved repository", err,
			float64(limit)/(1<<20))
	}
	return nil, errors.New(home.WorkspaceProblem(path, err, limit))
}

// startedRemote reports a session the cluster started or resumed and, unless
// --detach, attaches the terminal once it runs.
func (q *options) startedRemote(ctx context.Context, target remoteTarget, v work.View, verb string) error {
	attachNow := !q.detach
	noTerminal := attachNow && !term.IsTerminal(int(os.Stdin.Fd()))
	if noTerminal {
		// The session runs either way; only attaching needs a terminal.
		attachNow, q.detach = false, true
	}
	if q.json {
		if err := writeJSON(q.out, v); err != nil {
			return err
		}
	} else {
		line := fmt.Sprintf("%s session %s (%s) in cluster %s.", verb, v.ID, v.Harness, target.Name)
		if q.detach {
			line += fmt.Sprintf("\nConnect with: %s    Stop with: %s", q.remoteCommand(cmdAttach, v.ID),
				q.remoteCommand(cmdStop, v.ID))
		}
		if _, err := fmt.Fprintln(q.out, line); err != nil {
			return err
		}
	}
	if noTerminal {
		_, _ = fmt.Fprintf(os.Stderr, "Standard input is not a terminal, so this command did not attach; "+
			"run \"%s\" from a terminal\n", q.remoteCommand(cmdAttach, v.ID))
	}
	if !attachNow {
		return nil
	}
	return q.attachRemote(ctx, target, v.ID)
}

// attachRemote connects the terminal to a cluster's interactive session once
// it runs.
func (q *options) attachRemote(ctx context.Context, target remoteTarget, id string) error {
	if err := q.waitActive(ctx, target, id); err != nil {
		return err
	}
	return attachTerminal(ctx, id, q.remoteFlags(), q.remoteAttachment(target, id))
}

// waitActive waits while the session starts, and explains a session that is
// not running.
func (q *options) waitActive(ctx context.Context, target remoteTarget, id string) error {
	waiting := false
	for {
		v, err := target.Client.Info(ctx, id)
		if err != nil {
			return err
		}
		if v.Mode != work.ModeInteractive {
			return fmt.Errorf("session %s is a task, which runs without a terminal; see its progress and result "+
				"with \"%s\"", id, q.remoteCommand(cmdStatus, id))
		}
		switch string(v.Phase) {
		case phaseRunning:
			return nil
		case phaseStarting, phaseActivating:
		default:
			return q.phaseProblem(v)
		}
		if !waiting {
			waiting = true
			_, _ = fmt.Fprintf(os.Stderr, "Waiting for session %s to start in cluster %s; Ctrl-C stops waiting "+
				"and leaves it starting\n", shortID(id), target.Name)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("stopped waiting; session %s is still starting: attach later with \"%s\"", id,
				q.remoteCommand(cmdAttach, id))
		case <-time.After(remoteStartPoll):
		}
	}
}

// remoteAttachment attaches to a cluster's interactive session through its
// executor: the local terminal handling, over the executor's attachment.
func (q *options) remoteAttachment(target remoteTarget, id string) attachment {
	return func(ctx context.Context, input io.Reader, output io.Writer, sizes <-chan home.TerminalSize,
		attached func()) error {
		frames, write := io.Pipe()
		replies, err := target.Client.Attach(ctx, id, frames)
		if err != nil {
			_ = write.Close()
			return q.remoteActionError(ctx, target, id, cmdAttach, err)
		}
		attached()
		return home.AttachStream(ctx, id, q.remoteFlags(), &remoteStream{ReadCloser: replies, input: write},
			input, output, sizes)
	}
}

// remoteStream is an attachment to an executor: input frames go to the
// request body, output frames come from the response.
type remoteStream struct {
	io.ReadCloser
	input *io.PipeWriter
}

func (s *remoteStream) Write(p []byte) (int, error) { return s.input.Write(p) }
func (s *remoteStream) Close() error {
	_ = s.input.Close()
	return s.ReadCloser.Close()
}

// stopRemote stops a cluster's interactive session after the agent's
// current turn. While the turn runs it says what it waits for, and asks
// again with the same operation.
func (q *options) stopRemote(ctx context.Context, target remoteTarget) error {
	if q.operation == "" {
		q.operation = uuid.NewString()
	}
	noticed := false
	for {
		v, err := target.Client.Stop(ctx, q.id, q.operation)
		waiting := err == nil && string(v.Phase) == phaseStopping || timedOut(err)
		if err != nil && !waiting {
			return q.remoteActionError(ctx, target, q.id, cmdStop, err)
		}
		if !waiting {
			return q.printStopped(target.Name, v)
		}
		if !noticed {
			noticed = true
			_, _ = fmt.Fprintf(os.Stderr, "Waiting for the agent's current turn in session %s to finish; "+
				"run \"%s\" in another terminal to stop it now instead.\n", shortID(q.id),
				q.remoteCommand(cmdCancel, q.id))
		}
		select {
		case <-ctx.Done():
			return &retryError{err: errors.New("interrupted while the stop waited for the agent's current turn"),
				id: q.id, operation: q.operation}
		case <-time.After(remoteStopRetry):
		}
	}
}

// timedOut reports a request the executor did not answer in time, as a stop
// that waits for a long turn may be; the stop continues on the executor.
func timedOut(err error) bool {
	var remote *profile.RemoteError
	var timeout net.Error
	return errors.As(err, &remote) && remote.Transport != nil && errors.As(remote.Transport, &timeout) &&
		timeout.Timeout()
}

// printStopped reports a stopped session and its next commands.
func (q *options) printStopped(cluster string, v work.View) error {
	if q.json {
		return writeJSON(q.out, v)
	}
	text := fmt.Sprintf("Stopped session %s; its work is kept.", v.ID)
	if v.Phase == work.PhaseFailed && v.Exit.Failed() {
		text = fmt.Sprintf("Session %s had already stopped: its agent exited with code %d.%s", v.ID, v.Exit.Code,
			exitOutput(v.Exit))
	}
	text += fmt.Sprintf("\nContinue it in cluster %s with: %s    Bring it here with: %s", cluster,
		q.remoteCommand(cmdResume, v.ID), q.pullCommand(v.ID))
	_, err := fmt.Fprintln(q.out, text)
	return err
}

// resumeRemote starts a new execution of a cluster's stopped interactive
// session and attaches unless --detach. Each resume is a new operation.
func (q *options) resumeRemote(ctx context.Context, target remoteTarget) error {
	if q.operation == "" {
		q.operation = uuid.NewString()
	}
	v, err := target.Client.Resume(ctx, q.id, q.operation, q.maxDuration)
	if err != nil {
		return q.remoteActionError(ctx, target, q.id, cmdResume, err)
	}
	return q.startedRemote(ctx, target, v, "Resumed")
}

// remoteActionError explains a refused attach, stop or resume: an executor
// too old for it, a task, or a session in a phase that does not allow it.
// Only a request whose outcome is unknown names the operation to repeat.
func (q *options) remoteActionError(ctx context.Context, target remoteTarget, id, action string, err error) error {
	var remote *profile.RemoteError
	if !errors.As(err, &remote) {
		return err
	}
	switch {
	case remote.Code == profile.CodeUnavailable && remote.Status == http.StatusNotFound:
		// An executor older than interactive sessions has no such route.
		return fmt.Errorf("the cluster's remote executor %s is older than remote %s; update the executor "+
			"(docs/reference/remote-executors.md), or bring the session here with \"%s\"", target.Name, action,
			q.pullCommand(id))
	case remote.Code == profile.CodeUnsupported:
		return fmt.Errorf("session %s is a task, which stops by itself when it completes; see it with \"%s\", "+
			"or cancel it with \"%s\"", id, q.remoteCommand(cmdStatus, id), q.remoteCommand(cmdCancel, id))
	case remote.Code == profile.CodeConflict:
		if v, infoErr := target.Client.Info(ctx, id); infoErr == nil {
			if problem := q.phaseProblem(v); problem != nil {
				return problem
			}
		}
		return err
	case remote.Code == profile.CodeUnavailable && q.operation != "" && action != cmdAttach:
		return &retryError{err: err, id: id, operation: q.operation}
	}
	return err
}

// phaseProblem explains why a session in its phase cannot be attached to,
// stopped or resumed, naming the command that moves it on.
func (q *options) phaseProblem(v work.View) error {
	id := v.ID
	switch string(v.Phase) {
	case phaseStarting, phaseActivating:
		return fmt.Errorf("session %s is still starting; attach to it with \"%s\", or cancel it with \"%s\"", id,
			q.remoteCommand(cmdAttach, id), q.remoteCommand(cmdCancel, id))
	case phaseRunning:
		return fmt.Errorf("session %s is running; attach to it with \"%s\", or stop it with \"%s\" before "+
			"resuming", id, q.remoteCommand(cmdAttach, id), q.remoteCommand(cmdStop, id))
	case phaseStopping:
		return fmt.Errorf("session %s is still stopping; run \"%s\" to keep waiting, or \"%s\" to force-stop it",
			id, q.remoteCommand(cmdStop, id), q.remoteCommand(cmdCancel, id))
	case phaseUnknownStop, phaseUnknownUsage, phaseCancelling:
		return fmt.Errorf("session %s has unfinished work (%s); run \"%s\" to finish it", id, v.Phase,
			q.remoteCommand(cmdRecover, id))
	case string(work.PhaseRetained):
		return fmt.Errorf("session %s is stopped; \"%s\" continues it", id, q.remoteCommand(cmdResume, id))
	case string(work.PhaseFailed):
		if v.Exit.Failed() {
			return fmt.Errorf("session %s stopped because its agent exited with code %d; see \"%s\", then \"%s\" "+
				"to continue it", id, v.Exit.Code, q.remoteCommand(cmdStatus, id), q.remoteCommand(cmdResume, id))
		}
		return fmt.Errorf("session %s failed; see \"%s\"", id, q.remoteCommand(cmdStatus, id))
	case phaseCancelled:
		return fmt.Errorf("session %s was cancelled, so it cannot be stopped or resumed; start new work with "+
			"\"sproozi run --remote\"", id)
	case string(work.PhaseExporting), string(work.PhasePulled):
		return fmt.Errorf("session %s was pulled to a machine; continue it there with \"sproozi resume %s\"", id,
			shortID(id))
	}
	return nil
}
