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
	"strings"
	"syscall"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/session/profile"
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
	fixed                                                                   home.Config
	project                                                                 project.ID
	workspaceFrom                                                           *work.RevisionSource
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
	executor := fs.String("executor", "", "configured remote executor; resume uses docker")
	source := fs.String("from", "", "configured remote source for pull")
	kind := fs.String("harness", "", "approved native harness; defaults to global configuration")
	repository := fs.String("repository", "", "approved repository name")
	projectID := fs.String("project", "", "configured project; run defaults to its workspace, sessions filters by it")
	workspaceFrom := fs.String("workspace-from", "", "retained source session for fresh work at recorded HEAD")
	checkpointDigest := fs.String("checkpoint-digest", "", "exact source checkpoint digest for --workspace-from")
	revision := fs.String("revision", "", "exact full recorded Git commit for --workspace-from")
	task := fs.String("task", "", "task text")
	taskFile := fs.String("task-file", "", "UTF-8 task file")
	detach := fs.Bool("detach", false, "return after local resume without attaching")
	parseArgs := args[1:]
	if len(parseArgs) > 0 && !strings.HasPrefix(parseArgs[0], "-") {
		*id = parseArgs[0]
		parseArgs = parseArgs[1:]
	}
	if e = fs.Parse(parseArgs); e != nil {
		return options{}, e
	}
	if fs.NArg() != 0 {
		return options{}, errors.New("unexpected arguments")
	}
	var fixed home.Config
	needsConfig := action == actionRun || *socket == "" ||
		*executor != "" && *executor != dockerExecutor
	if needsConfig {
		fixed, e = home.ReadConfig(*config)
		if e != nil {
			return options{}, e
		}
		if *socket == "" {
			*socket = home.SocketPath(fixed.Directory)
		}
	}
	if *taskFile != "" {
		if *task != "" {
			return options{}, errors.New("choose --task or --task-file")
		}
		f, readErr := os.Open(*taskFile)
		if readErr != nil {
			return options{}, readErr
		}
		b, readErr := io.ReadAll(io.LimitReader(f, (16<<10)+1))
		if readErr = errors.Join(readErr, f.Close()); readErr != nil {
			return options{}, readErr
		}
		if len(b) > 16<<10 {
			return options{}, errors.New("task exceeds 16 KiB")
		}
		*task = string(b)
	}
	if action != actionRun && (*task != "" || *repository != "" || *kind != "") {
		return options{}, errors.New("task, repository and harness are run options")
	}
	if *projectID != "" && (action != actionRun && action != home.ActionSessions ||
		!project.ValidID(project.ID(*projectID))) {
		return options{}, errors.New("--project requires a valid project ID and run or sessions")
	}
	from, e := parseRevisionSource(action, *workspaceFrom, *checkpointDigest, *revision)
	if e != nil {
		return options{}, e
	}
	return options{action: action, socket: *socket, id: *id, operation: *operation, executor: *executor, source: *source,
		kind: *kind, repository: *repository, task: *task, detach: *detach, fixed: fixed,
		project: project.ID(*projectID), workspaceFrom: from}, nil
}

func parseRevisionSource(action, source, digest, revision string) (*work.RevisionSource, error) {
	if source == "" && digest == "" && revision == "" {
		return nil, nil
	}
	if action != actionRun || source == "" || digest == "" || revision == "" {
		return nil, errors.New("run --workspace-from requires --checkpoint-digest and --revision")
	}
	return &work.RevisionSource{SessionID: source, Digest: digest, Revision: revision}, nil
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
		if q.id != "" || q.operation != "" || q.source != "" {
			return errors.New("sessions accepts only configuration, socket and executor")
		}
		if q.executor != "" && q.executor != dockerExecutor {
			return remoteProduct(ctx, q.fixed, q.executor, q.action, "", "", q.project)
		}
		for _, name := range selectedExecutors(q.fixed, q.executor) {
			if e = remoteProduct(ctx, q.fixed, name, q.action, "", "", q.project); e != nil {
				return e
			}
		}
		if _, err := os.Stat(q.socket); os.IsNotExist(err) {
			return nil
		} else if err != nil {
			return err
		}
		response, err := home.Call(ctx, q.socket, home.Request{Action: home.ActionSessions, Project: q.project})
		if err != nil {
			return err
		}
		return json.NewEncoder(os.Stdout).Encode(response.Sessions)
	}
	if q.id == "" {
		return errors.New("stable session UUID required")
	}
	if (q.action == home.ActionStatus || q.action == home.ActionCancel || q.action == home.ActionRecover) &&
		q.executor != "" && q.executor != dockerExecutor {
		if q.action == home.ActionCancel && q.operation == "" {
			q.operation = uuid.NewString()
		}
		if q.action == home.ActionCancel {
			_, _ = fmt.Fprintf(os.Stderr, "Session %s, operation %s\n", q.id, q.operation)
		}
		return remoteProduct(ctx, q.fixed, q.executor, q.action, q.id, q.operation, "")
	}
	return q.local(ctx)
}

func (q *options) submitRemote(ctx context.Context) error {
	if q.id != "" || q.source != "" || q.executor == "" || q.executor == dockerExecutor {
		return errors.New("run requires an explicitly configured remote --executor")
	}
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
		Harness: harness.NativeKind(q.kind), Executor: q.executor, Task: q.task, Project: q.project,
		WorkspaceFrom: q.workspaceFrom})
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
	if q.action == home.ActionResume && q.executor != "" && q.executor != dockerExecutor {
		return errors.New("resume requires local Docker; use move for an active remote visit")
	}
	if q.action == home.ActionPull || q.action == home.ActionResume ||
		q.action == home.ActionStop || q.action == home.ActionCancel {
		if q.operation == "" {
			q.operation = uuid.NewString()
		}
		_, _ = fmt.Fprintf(os.Stderr, "Session %s, operation %s\n", q.id, q.operation)
	}
	request := home.Request{Action: q.action, ID: q.id, OperationID: q.operation, Source: q.source}
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
		names = append(names, name)
	}
	for name := range c.WorkSources {
		if _, exists := c.Remote[name]; !exists {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}
func remoteProduct(ctx context.Context, c home.Config, name, action, id, operation string, projectID project.ID) error {
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
		out, e = client.List(ctx, work.SessionQuery{Project: projectID})
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
