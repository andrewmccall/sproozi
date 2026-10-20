package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/localsetup"
	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/work"
	"github.com/google/uuid"
)

const (
	dockerExecutor = home.ReservedExecutor
	actionRun      = "run"
)

// defaultConfigPath keeps an existing platform configuration authoritative and
// otherwise uses the local installation root's configuration.
func defaultConfigPath() (string, error) { return localsetup.DefaultConfigPath() }

// setupOptions holds local provisioning and maintenance flags.
type setupOptions struct {
	enabled, credentialStdin, skipCheck, noService, json, leaveRunning, checkProvider bool
	root, model, image, build, providerURL, providerCA, credentialFile                string
	port                                                                              uint16
	timeout                                                                           time.Duration
	verb, operand                                                                     string
}

type options struct {
	setup                                                                   setupOptions
	action, socket, id, operation, executor, source, kind, repository, task string
	detach                                                                  bool
	// socketGiven reports --socket on the command line, not derived from config.
	socketGiven                                   bool
	workspace, destination, configPath, directory string
	includeIgnored                                []string
	fixed                                         home.Config
	project                                       project.ID
	workspaceFrom                                 *work.RevisionSource
	out                                           io.Writer
}

// parseProduct parses one command line, args[0] naming the command. It returns
// flag.ErrHelp when help was requested.
func parseProduct(args []string) (options, error) {
	c, ok := findCommand(args[0])
	if !ok {
		return options{}, unknownCommand(args[0])
	}
	if _, e := defaultConfigPath(); e != nil && c.name != actionServe && c.name != actionVersion && c.name != actionInit {
		return options{}, e
	}
	q := options{action: c.name}
	var in inputs
	fs := c.flagSet(&q, &in)
	rest := args[1:]
	if c.verbs != nil {
		var err error
		if rest, err = parseVerb(c, rest, &q.setup); err != nil {
			return options{}, err
		}
	}
	if err := parseSessionArguments(c, fs, rest, &q.id); err != nil {
		return options{}, err
	}
	configGiven := false
	fs.Visit(func(f *flag.Flag) { configGiven = configGiven || f.Name == configFlag })
	if configGiven && strings.TrimSpace(in.config) == "" {
		return options{}, errors.New("--config needs a path")
	}
	// Relative paths name files from the working directory, as init's
	// --directory does; home requires absolute paths.
	var e error
	if q.configPath, e = absolutePath(in.config); e != nil {
		return options{}, e
	}
	// On Windows --socket names a named pipe, not a file, so it is used as given.
	if runtime.GOOS != goosWindows {
		if q.socket, e = absolutePath(q.socket); e != nil {
			return options{}, e
		}
	}
	if err := parseSetupInputs(in, &q.setup); err != nil {
		return options{}, err
	}
	if isSetupCommand(c.name) || c.name == actionVersion {
		return q, nil
	}
	if c.name == actionServe {
		if q.configPath == "" {
			return options{}, errors.New("serve requires --config FILE: no platform configuration directory")
		}
		return q, nil
	}
	q.project = project.ID(in.project)
	if q.project != "" && !project.ValidID(q.project) {
		return options{}, errors.New("--project requires a valid project ID")
	}
	q.workspaceFrom, e = parseRevisionSource(in.workspaceFrom, in.checkpointDigest, in.revision)
	if e != nil {
		return options{}, e
	}
	if err := q.prepareAddress(c); err != nil {
		return options{}, err
	}
	if err := q.loadConfiguration(fs); err != nil {
		return options{}, err
	}
	if err := q.checkExecutor(); err != nil {
		return options{}, err
	}
	q.task, e = readTaskText(q.task, in.taskFile)
	return q, e
}

func (q *options) loadConfiguration(fs *flag.FlagSet) error {
	q.socketGiven = q.socket != ""
	needsConfig := q.action == actionRun || q.socket == "" || q.executor != "" && q.executor != dockerExecutor
	fs.Visit(func(f *flag.Flag) {
		if f.Name == configFlag {
			needsConfig = true
		}
	})
	if !needsConfig {
		return nil
	}
	fixed, err := readConfig(q.configPath)
	if err != nil {
		return err
	}
	q.fixed = fixed
	if q.socket == "" {
		q.socket = home.SocketPath(fixed.Directory)
	}
	return nil
}

func readTaskText(task, path string) (string, error) {
	if path == "" {
		return task, nil
	}
	if task != "" {
		return "", errors.New("choose --task or --task-file")
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	b, err := io.ReadAll(io.LimitReader(f, (16<<10)+1))
	if err = errors.Join(err, f.Close()); err != nil {
		return "", err
	}
	if len(b) > 16<<10 {
		return "", errors.New("task exceeds 16 KiB")
	}
	return string(b), nil
}

func parseRevisionSource(source, digest, revision string) (*work.RevisionSource, error) {
	if source == "" && digest == "" && revision == "" {
		return nil, nil
	}
	if source == "" || digest == "" || revision == "" {
		return nil, errors.New("run --workspace-from requires --checkpoint-digest and --revision")
	}
	return &work.RevisionSource{SessionID: source, Digest: digest, Revision: revision}, nil
}

func runProduct(args []string, stdout io.Writer) error {
	q, e := parseProduct(args)
	if errors.Is(e, flag.ErrHelp) {
		c, _ := findCommand(args[0])
		c.usage(stdout)
		return nil
	}
	if e != nil {
		return e
	}
	q.out = stdout
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	switch {
	case q.action == actionInit && q.setup.enabled:
		return q.initLocal(ctx)
	case q.action == actionInit:
		return initialize(q.directory, stdout)
	case isSetupCommand(q.action):
		return q.maintain(ctx)
	case q.action == actionVersion:
		_, err := fmt.Fprintln(stdout, build)
		return err
	case q.action == actionServe:
		return serve(ctx, q.configPath)
	case q.action == actionRun:
		return q.submitRemote(ctx)
	case q.action == home.ActionSessions:
		if err := q.checkSocket(); err != nil {
			return err
		}
		found := discoverSessions(ctx, q)
		if err := json.NewEncoder(q.out).Encode(found); err != nil {
			return err
		}
		return found.failure(q.configPath)
	case q.executor != "" && q.executor != dockerExecutor && q.action != home.ActionPull:
		return q.remote(ctx)
	}
	return q.local(ctx)
}

func (q *options) submitRemote(ctx context.Context) error {
	remote, ok := q.fixed.WorkConnections()[q.executor]
	if !ok {
		return q.unconfigured()
	}
	client, err := work.NewClient(remote)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	if q.operation == "" {
		q.operation = uuid.NewString()
	}
	if q.kind == "" {
		q.kind = string(q.fixed.Local.Harness)
	}
	v, err := client.Submit(ctx, work.Submit{RequestID: q.operation, Repository: q.repository,
		Harness: harness.NativeKind(q.kind), Executor: q.executor, Task: q.task, Project: q.project,
		WorkspaceFrom: q.workspaceFrom})
	if err != nil {
		return fmt.Errorf("%w; to retry, repeat the command with --operation %s", err, q.operation)
	}
	_, _ = fmt.Fprintf(os.Stderr, "Run operation %s\n", q.operation)
	return json.NewEncoder(q.out).Encode(v)
}

func (q *options) local(ctx context.Context) error {
	switch q.action {
	case home.ActionStart, home.ActionMove, home.ActionPull, home.ActionResume, home.ActionStop, home.ActionCancel:
		if q.operation == "" {
			q.operation = uuid.NewString()
		}
	}
	if err := q.checkSocket(); err != nil {
		return err
	}
	if err := q.ensureService(ctx); err != nil {
		return err
	}
	if q.action == home.ActionAttach {
		return attach(ctx, q.socket, q.id)
	}
	request := home.Request{
		Action: q.action, ID: q.id, OperationID: q.operation, Source: q.source,
		Workspace: q.workspace, Destination: q.destination, IncludeIgnored: q.includeIgnored,
		Harness: harness.NativeKind(q.kind),
	}
	response, err := home.Call(ctx, q.socket, request)
	// A refused or failed request prints only its error, not the session.
	if response.Status != nil && err == nil {
		if e := json.NewEncoder(q.out).Encode(response.Status); e != nil {
			return e
		}
	}
	if err != nil {
		// Repeating a request for a session home does not retain, or one it
		// rejected before recording, cannot help.
		if q.operation != "" && !errors.Is(err, home.ErrUnknownSession) && !errors.Is(err, home.ErrRejected) {
			return &retryError{err: err, id: q.id, operation: q.operation}
		}
		return err
	}
	q.reportIDs()
	if (q.action == home.ActionResume || q.action == home.ActionStart) && !q.detach {
		return attach(ctx, q.socket, q.id)
	}
	return nil
}

// reportIDs prints the stable IDs of a mutation that succeeded.
func (q *options) reportIDs() {
	if q.operation != "" {
		_, _ = fmt.Fprintf(os.Stderr, "Session %s, operation %s\n", q.id, q.operation)
	}
}

// checkExecutor rejects a remote executor the configuration does not name
// before any request is sent.
func (q *options) checkExecutor() error {
	if q.action == home.ActionMove {
		if err := q.checkDestination(); err != nil {
			return err
		}
	}
	// A configuration that was read also names the executors pull may take.
	if _, ok := q.fixed.WorkConnections()[q.source]; q.action == home.ActionPull && q.fixed.Directory != "" && !ok {
		return fmt.Errorf("pull --from %q names no remote executor configured under %s in %s; see %s",
			q.source, remoteKeys, q.configPath, remoteSetupDoc)
	}
	if q.executor == "" || q.executor == dockerExecutor {
		return nil
	}
	if _, ok := q.fixed.WorkConnections()[q.executor]; ok {
		return nil
	}
	return q.unconfigured()
}

func (q *options) unconfigured() error {
	if q.action == actionRun {
		what := "is not configured"
		if q.executor == q.fixed.Local.Name {
			what = "is local.name, home's local placement for \"move --to\", not an executor"
		} else if problem := home.ExecutorNameProblem(q.executor); problem != "" {
			what = "cannot name an executor: " + problem
		}
		return fmt.Errorf("run submits only to a remote executor configured under %s in %s; "+
			"%q %s; see %s", remoteKeys, q.configPath, q.executor, what, remoteSetupDoc)
	}
	if q.executor == q.fixed.Local.Name {
		return fmt.Errorf("executor %q is local.name, home's local placement for \"move --to\", not an executor; %s",
			q.executor, useDocker)
	}
	if problem := home.ExecutorNameProblem(q.executor); problem != "" {
		return fmt.Errorf("executor %s, so no executor can have it; use --executor docker (home's executor name) "+
			"or name an executor configured under %s in %s", problem, remoteKeys, q.configPath)
	}
	return fmt.Errorf("executor %q is not configured; use --executor docker (home's executor name), "+
		"or add it under %s in %s, see %s", q.executor, remoteKeys, q.configPath, configDoc)
}

// checkDestination rejects a move destination that is not a configured
// placement before any request is sent. Executor names are not placements.
func (q *options) checkDestination() error {
	local := q.fixed.Local.Name
	if local == "" {
		local = "<local.name value>"
	}
	switch q.destination {
	case dockerExecutor:
		return fmt.Errorf("move --to docker: \"docker\" is home's executor name; "+
			"use --to %s (local.name) to move to this machine", local)
	case home.ReservedHome:
		return fmt.Errorf("move --to home: \"home\" names the home daemon; "+
			"use --to %s (local.name) to move to this machine", local)
	}
	// Without a read configuration the daemon checks the name.
	if q.fixed.Directory == "" || q.destination == local {
		return nil
	}
	if _, ok := q.fixed.Remote[q.destination]; ok {
		return nil
	}
	valid := []string{local + " (local.name)"}
	for _, name := range slices.Sorted(maps.Keys(q.fixed.Remote)) {
		valid = append(valid, name+" (remote."+name+")")
	}
	return fmt.Errorf("move --to %q is not a configured placement; use one of: %s, or add it under \"remote\" in %s",
		q.destination, strings.Join(valid, ", "), q.configPath)
}

// remoteKeys names the configuration maps that define remote executors.
const remoteKeys = `"remote" or "workSources"`

// unknownCommand points to help, and from doctor to the repository's setup check.
func unknownCommand(name string) error {
	err := fmt.Errorf("unknown command %q; run \"sproozi -h\" for commands", name)
	if name == "doctor" {
		err = fmt.Errorf("%w. The setup check is \"python3 hack/verify/quickstart.py doctor\" "+
			"in a repository checkout, see %s", err, docURL("docs/guides/getting-started.md"))
	}
	return err
}

// selectedExecutors names the remote catalogues discovery queries by default.
func selectedExecutors(c home.Config, selected string) []string {
	if selected == dockerExecutor {
		return nil
	}
	names := []string{}
	for name := range c.WorkConnections() {
		if name != dockerExecutor {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// remote sends status, cancel or recover to the selected remote owner.
func (q *options) remote(ctx context.Context) error {
	fixed, ok := q.fixed.WorkConnections()[q.executor]
	if !ok {
		return q.unconfigured()
	}
	client, e := work.NewClient(fixed)
	if e != nil {
		return e
	}
	defer func() { _ = client.Close() }()
	var out any
	switch q.action {
	case home.ActionStatus:
		out, e = client.Info(ctx, q.id)
	case home.ActionCancel:
		if q.operation == "" {
			q.operation = uuid.NewString()
		}
		out, e = client.Cancel(ctx, q.id, q.operation)
		if e != nil {
			return &retryError{err: e, id: q.id, operation: q.operation}
		}
		q.reportIDs()
	case home.ActionRecover:
		out, e = client.Recover(ctx, q.id)
	default:
		return errors.New("unsupported remote action")
	}
	if e != nil {
		return e
	}
	return json.NewEncoder(q.out).Encode(out)
}

// absolutePath resolves a path flag against the working directory.
func absolutePath(path string) (string, error) {
	if path == "" || filepath.IsAbs(path) {
		return path, nil
	}
	return filepath.Abs(path)
}

// checkSocket explains an explicit --socket that cannot work: too long to
// bind, or naming something that is not a socket. A socket derived from
// config.json names its "directory" when dialled instead.
func (q *options) checkSocket() error {
	if !q.socketGiven {
		return nil
	}
	if problem := home.SocketPathProblem(q.socket); problem != "" {
		return errors.New(problem + "; use a shorter --socket path")
	}
	if info, err := os.Stat(q.socket); err == nil && info.Mode()&os.ModeSocket == 0 && runtime.GOOS != goosWindows {
		kind := "a regular file"
		if info.IsDir() {
			kind = "a directory"
		}
		return fmt.Errorf("--socket %s is %s, not a socket; name home's control.sock", shellQuote(q.socket), kind)
	}
	return nil
}
