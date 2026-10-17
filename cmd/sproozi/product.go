package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"syscall"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/work"
	"github.com/google/uuid"
)

const (
	dockerExecutor = "docker"
	actionRun      = "run"
)

func defaultConfigPath() (string, error) {
	p, e := os.UserConfigDir()
	return filepath.Join(p, "sproozi", "config.json"), e
}

type options struct {
	action, socket, id, operation, executor, source, kind, repository, task string
	detach                                                                  bool
	workspace, destination, configPath                                      string
	includeIgnored                                                          []string
	fixed                                                                   home.Config
	project                                                                 project.ID
	workspaceFrom                                                           *work.RevisionSource
	out                                                                     io.Writer
}

// parseProduct parses one command line, args[0] naming the command. It returns
// flag.ErrHelp when help was requested.
func parseProduct(args []string) (options, error) {
	c, ok := findCommand(args[0])
	if !ok {
		return options{}, fmt.Errorf("unknown command %q; run \"sproozi -h\" for commands", args[0])
	}
	if _, e := defaultConfigPath(); e != nil && c.name != actionServe {
		return options{}, e
	}
	q := options{action: c.name}
	var in inputs
	fs := c.flagSet(&q, &in)
	if err := parseSessionArguments(c, fs, args[1:], &q.id); err != nil {
		return options{}, err
	}
	q.configPath = in.config
	if c.name == actionServe {
		if q.configPath == "" {
			return options{}, errors.New("serve requires --config FILE")
		}
		return q, nil
	}
	q.project = project.ID(in.project)
	if q.project != "" && !project.ValidID(q.project) {
		return options{}, errors.New("--project requires a valid project ID")
	}
	var e error
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
	q.task, e = readTaskText(q.task, in.taskFile)
	return q, e
}

func (q *options) loadConfiguration(fs *flag.FlagSet) error {
	needsConfig := q.action == actionRun || q.socket == "" || q.executor != "" && q.executor != dockerExecutor
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			needsConfig = true
		}
	})
	if !needsConfig {
		return nil
	}
	fixed, err := home.ReadConfig(q.configPath)
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
	case q.action == actionServe:
		return serve(ctx, q.configPath)
	case q.action == actionRun:
		return q.submitRemote(ctx)
	case q.action == home.ActionSessions:
		return json.NewEncoder(q.out).Encode(discoverSessions(ctx, q))
	case q.executor != "" && q.executor != dockerExecutor && q.action != home.ActionPull:
		return q.remote(ctx)
	}
	return q.local(ctx)
}

func (q *options) submitRemote(ctx context.Context) error {
	remote, ok := q.fixed.WorkConnections()[q.executor]
	if !ok {
		return errors.New("executor is not configured")
	}
	client, err := work.NewClient(remote)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	if q.operation == "" {
		q.operation = uuid.NewString()
	}
	_, _ = fmt.Fprintf(os.Stderr, "Run operation %s\n", q.operation)
	if q.kind == "" {
		q.kind = string(q.fixed.Local.Harness)
	}
	v, err := client.Submit(ctx, work.Submit{RequestID: q.operation, Repository: q.repository,
		Harness: harness.NativeKind(q.kind), Executor: q.executor, Task: q.task, Project: q.project,
		WorkspaceFrom: q.workspaceFrom})
	if err != nil {
		return err
	}
	return json.NewEncoder(q.out).Encode(v)
}

func (q *options) local(ctx context.Context) error {
	switch q.action {
	case home.ActionStart, home.ActionMove, home.ActionPull, home.ActionResume, home.ActionStop, home.ActionCancel:
		if q.operation == "" {
			q.operation = uuid.NewString()
		}
		_, _ = fmt.Fprintf(os.Stderr, "Session %s, operation %s\n", q.id, q.operation)
	}
	if q.action == home.ActionAttach {
		return attach(ctx, q.socket, q.id)
	}
	request := home.Request{
		Action: q.action, ID: q.id, OperationID: q.operation, Source: q.source,
		Workspace: q.workspace, Destination: q.destination, IncludeIgnored: q.includeIgnored,
	}
	response, err := home.Call(ctx, q.socket, request)
	if response.Status != nil {
		if e := json.NewEncoder(q.out).Encode(response.Status); e != nil {
			return e
		}
	}
	if err != nil {
		return err
	}
	if q.action == home.ActionResume && !q.detach {
		return attach(ctx, q.socket, q.id)
	}
	return nil
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
		return errors.New("remote executor is not configured")
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
		_, _ = fmt.Fprintf(os.Stderr, "Session %s, operation %s\n", q.id, q.operation)
		out, e = client.Cancel(ctx, q.id, q.operation)
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
