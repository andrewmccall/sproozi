package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/agentprofile"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/localsetup"
	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/work"
	"github.com/google/uuid"
	"golang.org/x/term"
)

// homeActions maps the commands this machine's service runs to its requests.
var homeActions = map[string]string{
	cmdRun: home.ActionStart, cmdStatus: home.ActionStatus, cmdAttach: home.ActionAttach, cmdStop: home.ActionStop,
	cmdResume: home.ActionResume, cmdCancel: home.ActionCancel, cmdRemove: home.ActionRemove, cmdMove: home.ActionMove,
	cmdPull: home.ActionPull, cmdRecover: home.ActionRecover,
}

// mutations change state and carry an operation ID, so a request whose
// outcome is unknown can be repeated exactly.
var mutations = []string{cmdRun, cmdStop, cmdResume, cmdCancel, cmdMove, cmdPull}

// session runs a session command: on a cluster when --context or --namespace
// names one (pull always imports to this machine), otherwise on this machine.
func (q *options) session(ctx context.Context) error {
	if q.remote && q.action() != cmdPull && q.action() != cmdMove {
		return q.remoteSession(ctx)
	}
	if err := q.load(); err != nil {
		return err
	}
	if q.action() == cmdAttach && !term.IsTerminal(int(os.Stdin.Fd())) {
		return errNoTerminal
	}
	if q.action() == cmdRun {
		if err := q.prepareRun(); err != nil {
			return err
		}
	}
	if slices.Contains(mutations, q.action()) && q.action() != cmdPull && q.operation == "" {
		q.operation = uuid.NewString()
	}
	if err := q.ensureService(ctx); err != nil {
		return err
	}
	if err := q.resolveID(ctx); err != nil {
		return err
	}
	request, done, err := q.request(ctx)
	if done || err != nil {
		return err
	}
	stopNotice := waitNotice(q)
	response, err := home.Call(ctx, q.socket, request)
	stopNotice()
	if err != nil {
		return q.callError(ctx, err)
	}
	if response.Status == nil {
		return errors.New("the service sent no session status")
	}
	attachNow := (q.action() == cmdRun || q.action() == cmdResume) && !q.detach
	noTerminal := attachNow && !term.IsTerminal(int(os.Stdin.Fd()))
	if noTerminal {
		// The session runs either way; only attaching needs a terminal.
		attachNow, q.detach = false, true
	}
	if err = q.printStatus(*response.Status); err != nil {
		return err
	}
	if noTerminal {
		_, _ = fmt.Fprintf(os.Stderr, "Standard input is not a terminal, so this command did not attach; "+
			"run \"sproozi attach %s\" from a terminal\n", shortID(q.id))
	}
	if attachNow {
		return q.homeError(attach(ctx, q.socket, q.id))
	}
	return nil
}

// callError explains a failed request. Only one whose outcome is unknown,
// including one interrupted before the reply, is worth repeating, with the
// same session and operation.
func (q *options) callError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		err = errors.New("interrupted before the service replied")
	}
	if q.operation != "" && !errors.Is(err, home.ErrUnknownSession) && !errors.Is(err, home.ErrRejected) &&
		!errors.Is(err, home.ErrControlUnavailable) {
		return &retryError{err: q.homeError(err), id: q.id, operation: q.operation, newSession: q.action() == cmdRun}
	}
	return q.homeError(err)
}

// request builds the service request. done reports a command that finished
// without one: attach, or cancelling a session already cancelled.
func (q *options) request(ctx context.Context) (home.Request, bool, error) {
	request := home.Request{Action: homeActions[q.action()], ID: q.id, OperationID: q.operation,
		IncludeIgnored: q.includeIgnored, Harness: harness.NativeKind(q.harness), HarnessArgs: q.harnessArgs, Login: q.login}
	switch q.action() {
	case cmdRun:
		request.Workspace = q.path
	case cmdAttach:
		return request, true, q.homeError(attach(ctx, q.socket, q.id))
	case cmdMove:
		if q.local {
			request.Destination = home.LocalAlias
			break
		}
		selection, err := selectRemote(q.context, q.namespace)
		if err != nil {
			return request, true, err
		}
		request.Remote = &selection
	case cmdPull:
		// A pull the service already began continues from its own source, so
		// a retry needs no context. Otherwise the selection names the cluster.
		selection, err := selectRemote(q.context, q.namespace)
		if err != nil && q.remote {
			return request, true, err
		}
		if err == nil {
			request.Remote = &selection
		}
	case cmdCancel:
		// Cancelling a cancelled session succeeds without another request.
		if status, err := home.Call(ctx, q.socket, home.Request{Action: home.ActionStatus, ID: q.id}); err == nil &&
			status.Status != nil && status.Status.Phase == phaseCancelled {
			return request, true, q.printStatus(*status.Status)
		}
	}
	return request, false, nil
}

// prepareRun fills a local run's workspace, session ID and agent.
func (q *options) prepareRun() error {
	if q.path == "" {
		dir, err := os.Getwd()
		if err != nil {
			return err
		}
		q.path = dir
	}
	if q.id == "" {
		q.id = uuid.NewString()
	} else if !work.CanonicalID(q.id) {
		return usagef("--id must be a full session UUID")
	}
	if q.harness == "" || q.fixed.Directory == "" {
		return nil
	}
	configured := []string{string(q.fixed.Local.Harness)}
	for _, h := range q.fixed.Harnesses {
		configured = append(configured, string(h.Harness))
	}
	if !slices.Contains(configured, q.harness) {
		return fmt.Errorf("%s is not set up on this machine; add it with \"sproozi init --harness %s\"", q.harness, q.harness)
	}
	return nil
}

// homeError names the service's state in plain words.
func (q *options) homeError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, home.ErrControlUnavailable):
		command := startServiceCommand
		if q.configPath != "" {
			command = startCommand(q.configPath)
		}
		return &serviceDown{err: err, command: command}
	case errors.Is(err, home.ErrUnknownSession):
		return fmt.Errorf("no session %s on this machine; \"sproozi ls\" lists sessions", q.id)
	}
	return err
}

// resolveLocalID completes a session ID prefix from this machine's sessions.
func (q *options) resolveLocalID(ctx context.Context) error {
	if work.CanonicalID(q.id) {
		return nil
	}
	response, err := home.Call(ctx, q.socket, home.Request{Action: home.ActionSessions})
	if err != nil {
		return q.homeError(err)
	}
	ids := make([]string, 0, len(response.Sessions))
	for _, s := range response.Sessions {
		ids = append(ids, s.ID)
	}
	q.id, err = matchID(q.id, ids)
	return err
}

// resolveID completes a session ID prefix: from the cluster as well for pull,
// from this machine otherwise. run creates its ID.
func (q *options) resolveID(ctx context.Context) error {
	switch q.action() {
	case cmdRun:
		return nil
	case cmdPull:
		if err := q.resolvePullID(ctx); err != nil {
			return err
		}
		// One session has one pull, so a plain repeat continues it.
		if q.operation == "" {
			q.operation = uuid.NewSHA1(uuid.NameSpaceOID, []byte("sproozi/pull/"+q.id)).String()
		}
		return nil
	}
	return q.resolveLocalID(ctx)
}

// resolvePullID completes a session ID prefix for pull from the cluster's
// sessions as well as this machine's, since the session usually lives only in
// the cluster until the pull.
func (q *options) resolvePullID(ctx context.Context) error {
	if work.CanonicalID(q.id) {
		return nil
	}
	var ids []string
	if response, err := home.Call(ctx, q.socket, home.Request{Action: home.ActionSessions}); err == nil {
		for _, s := range response.Sessions {
			ids = append(ids, s.ID)
		}
	}
	target, err := resolveRemote(ctx, q.context, q.namespace)
	if err != nil {
		return fmt.Errorf("%w; or give the full session ID", err)
	}
	defer func() { _ = target.Client.Close() }()
	views, err := target.Client.List(ctx, work.SessionQuery{})
	if err != nil {
		return fmt.Errorf("cluster %s did not list its sessions to complete %s: %s; give the full session ID",
			target.Name, q.id, executorCause(err))
	}
	for _, v := range views {
		ids = append(ids, v.ID)
	}
	q.id, err = matchID(q.id, ids)
	return err
}

// matchID finds the one ID that starts with prefix.
func matchID(prefix string, ids []string) (string, error) {
	var matches []string
	for _, id := range ids {
		if strings.HasPrefix(id, prefix) && !slices.Contains(matches, id) {
			matches = append(matches, id)
		}
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no session ID starts with %s; \"sproozi ls\" lists sessions", prefix)
	}
	return "", fmt.Errorf("%s matches %d sessions (%s); give more of the ID", prefix, len(matches),
		strings.Join(matches, ", "))
}

// ensureService starts an installation's service when a session command
// finds it stopped. A hand-written configuration is left to its administrator.
func (q *options) ensureService(ctx context.Context) error {
	if q.configPath == "" || q.fixed.Directory == "" {
		return nil
	}
	l := localsetup.Layout{Root: filepath.Dir(q.configPath)}
	if _, err := os.Stat(l.Installation()); err != nil || l.Config() != q.configPath {
		return nil
	}
	if status := localsetup.ServiceState(ctx, l, q.fixed); status.Running {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(os.Stderr, "Starting the Sproozi service")
	_, err = localsetup.StartService(ctx, l, q.fixed, executable, 2*time.Minute)
	return err
}

// stopNoticeAfter is how long stop or move waits before saying why it waits.
var stopNoticeAfter = 30 * time.Second

// waitNotice tells the user, once stop or move has waited a while for the
// agent's current turn, what it waits for and how to stop it now instead.
// The returned function cancels the notice.
func waitNotice(q *options) func() {
	if q.action() != cmdStop && q.action() != cmdMove {
		return func() {}
	}
	timer := time.AfterFunc(stopNoticeAfter, func() {
		_, _ = fmt.Fprintf(os.Stderr, "Waiting for the agent's current turn in session %s to finish; "+
			"run \"sproozi cancel %s\" in another terminal to stop it now instead.\n", shortID(q.id), shortID(q.id))
	})
	return func() { timer.Stop() }
}

// remoteSession runs status, cancel or recover on a cluster.
func (q *options) remoteSession(ctx context.Context) error {
	if q.action() != cmdStatus && q.action() != cmdCancel && q.action() != cmdRecover {
		return usagef("%s works on this machine's sessions; it does not take --context or --namespace", q.action())
	}
	if err := q.loadOptional(); err != nil {
		return err
	}
	target, err := resolveRemote(ctx, q.context, q.namespace)
	if err != nil {
		return err
	}
	client := target.Client
	defer func() { _ = client.Close() }()
	if !work.CanonicalID(q.id) {
		views, err := client.List(ctx, work.SessionQuery{})
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(views))
		for _, v := range views {
			ids = append(ids, v.ID)
		}
		if q.id, err = matchID(q.id, ids); err != nil {
			return err
		}
	}
	var view work.View
	switch q.action() {
	case cmdStatus:
		view, err = client.Info(ctx, q.id)
	case cmdRecover:
		view, err = client.Recover(ctx, q.id)
	case cmdCancel:
		if view, err = client.Info(ctx, q.id); err == nil && view.Phase == phaseCancelled {
			break
		}
		if q.operation == "" {
			q.operation = uuid.NewString()
		}
		if view, err = client.Cancel(ctx, q.id, q.operation); err != nil {
			return &retryError{err: err, id: q.id, operation: q.operation}
		}
	}
	if err != nil {
		return err
	}
	return q.printView(target.Name, view)
}

// loadOptional reads the configuration when there is one; remote commands
// do not need an installation on this machine.
func (q *options) loadOptional() error {
	err := q.load()
	path, _ := q.config()
	if _, statErr := os.Stat(path); err != nil && os.IsNotExist(statErr) && q.configPath == "" {
		return nil
	}
	return err
}

// submitRemote starts a task in a cluster.
func (q *options) submitRemote(ctx context.Context) error {
	if err := q.loadOptional(); err != nil {
		return err
	}
	target, err := resolveRemote(ctx, q.context, q.namespace)
	if err != nil {
		return err
	}
	client := target.Client
	defer func() { _ = client.Close() }()
	if q.operation == "" {
		q.operation = uuid.NewString()
	}
	kind := harness.NativeKind(q.harness)
	if kind == "" {
		kind = q.fixed.Local.Harness
	}
	if kind == "" {
		kind = harness.NativeCodex
	}
	// The run receives the profile as it is now; a profile over the
	// cluster's limit refuses the run before anything is submitted.
	archive, _, err := agentprofile.ArchiveDirectory(q.fixed.Local.ProfileDirectory)
	if err != nil {
		return fmt.Errorf("profile: %w", err)
	}
	v, err := client.Submit(ctx, work.Submit{RequestID: q.operation, Repository: q.repository, Harness: kind,
		Executor: target.Name, Task: q.task, Project: q.project, WorkspaceFrom: q.workspaceFrom, Profile: archive})
	if err != nil {
		return fmt.Errorf("%w; the outcome is unknown: to try again, repeat the command with --operation %s",
			err, q.operation)
	}
	return q.printView(target.Name, v)
}
