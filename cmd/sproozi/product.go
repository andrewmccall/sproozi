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
	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/session/profile"
	"github.com/andrewmccall/sproozi/internal/work"
	"github.com/andrewmccall/sproozi/internal/workspace"
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
	workspace, destination                                                  string
	includeIgnored                                                          []string
	fixed                                                                   home.Config
}

func parseProduct(args []string) (options, error) {
	action := args[0]
	fs := flag.NewFlagSet("sproozi "+action, flag.ContinueOnError)
	configPath, e := defaultConfigPath()
	if e != nil {
		return options{}, e
	}
	config := fs.String("config", configPath, "private global Sproozi configuration")
	socket := fs.String("socket", "", "private home socket; defaults to configured home")
	id := fs.String("id", "", "stable session UUID; may also be the first positional argument")
	operation := fs.String("operation", "", "stable operation UUID; preserve on retries")
	executor := fs.String("executor", "", "configured executor; local control uses docker")
	source := fs.String("from", "", "configured remote source for pull")
	kind := fs.String("harness", "", "approved native harness; defaults to global configuration")
	repository := fs.String("repository", "", "approved repository name")
	task := fs.String("task", "", "task text")
	taskFile := fs.String("task-file", "", "UTF-8 task file")
	detach := fs.Bool("detach", false, "return after local resume without attaching")
	workspacePath := fs.String("workspace", "", "stopped ordinary Git directory for start")
	destination := fs.String("to", "", "configured destination for move")
	var includeIgnored []string
	fs.Func("include-ignored", "ignored file for every visit; repeatable start intent", func(v string) error {
		includeIgnored = append(includeIgnored, v)
		return workspace.ValidateIgnoredPaths(includeIgnored)
	})
	if err := parseSessionArguments(fs, args[1:], id); err != nil {
		return options{}, err
	}
	q := options{
		action: action, socket: *socket, id: *id, operation: *operation, executor: *executor, source: *source,
		kind: *kind, repository: *repository, task: *task, detach: *detach,
		workspace: *workspacePath, destination: *destination, includeIgnored: includeIgnored,
	}
	if err := q.prepareAddress(fs); err != nil {
		return options{}, err
	}
	if err := q.loadConfiguration(fs, *config); err != nil {
		return options{}, err
	}
	q.task, e = readTaskText(q.task, *taskFile)
	return q, e
}

func (q *options) loadConfiguration(fs *flag.FlagSet, path string) error {
	needsConfig := q.action == actionRun || q.socket == "" || q.executor != "" && q.executor != dockerExecutor
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "config" {
			needsConfig = true
		}
	})
	if !needsConfig {
		return nil
	}
	fixed, err := home.ReadConfig(path)
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

func runProduct(args []string) error {
	q, e := parseProduct(args)
	if e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if q.action == actionRun {
		return q.submitRemote(ctx)
	}
	if q.action == home.ActionSessions {
		catalogue := discoverSessions(ctx, q)
		return json.NewEncoder(os.Stdout).Encode(catalogue)
	}

	if (q.action == home.ActionStatus || q.action == home.ActionCancel || q.action == home.ActionRecover) &&
		q.executor != "" && q.executor != dockerExecutor {
		if q.action == home.ActionCancel && q.operation == "" {
			q.operation = uuid.NewString()
		}
		if q.action == home.ActionCancel {
			_, _ = fmt.Fprintf(os.Stderr, "Session %s, operation %s\n", q.id, q.operation)
		}
		return remoteProduct(ctx, q.fixed, q.executor, q.action, q.id, q.operation)
	}
	return q.local(ctx)
}

func (q *options) submitRemote(ctx context.Context) error {
	remote, ok := workConnection(q.fixed, q.executor)
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
		Harness: harness.NativeKind(q.kind), Executor: q.executor, Task: q.task})
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(v)
}

func (q *options) local(ctx context.Context) error {
	if q.action == home.ActionPull {
		if q.source == "" {
			q.source = q.executor
		}
		if q.source == "" {
			return errors.New("pull requires --from configured-source")
		}
	}
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
		if e := json.NewEncoder(os.Stdout).Encode(response.Status); e != nil {
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
func selectedExecutors(c home.Config, selected string) []string {
	if selected == dockerExecutor {
		return nil
	}
	names := make([]string, 0, len(c.Remote)+len(c.WorkSources))
	for name := range c.Remote {
		if name == dockerExecutor {
			continue
		}
		names = append(names, name)
	}
	for name := range c.WorkSources {
		if name == dockerExecutor {
			continue
		}
		if _, exists := c.Remote[name]; !exists {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}
func remoteProduct(ctx context.Context, c home.Config, name, action, id, operation string) error {
	fixed, ok := workConnection(c, name)
	if !ok {
		return errors.New("remote executor is not configured")
	}
	client, e := work.NewClient(fixed)
	if e != nil {
		return e
	}
	defer func() { _ = client.Close() }()
	var out any
	switch action {
	case home.ActionSessions:
		out, e = client.List(ctx)
	case home.ActionStatus:
		out, e = client.Info(ctx, id)
	case home.ActionCancel:
		out, e = client.Cancel(ctx, id, operation)
	case home.ActionRecover:
		out, e = client.Recover(ctx, id)
	default:
		return errors.New("unsupported remote action")
	}
	if e != nil {
		return e
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}

func workConnection(c home.Config, name string) (profile.ClientConfig, bool) {
	if fixed, ok := c.WorkSources[name]; ok {
		return fixed, true
	}
	fixed, ok := c.Remote[name]
	return fixed, ok
}
