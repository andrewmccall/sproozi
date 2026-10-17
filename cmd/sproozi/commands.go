package main

import (
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/workspace"
)

const actionServe = "serve"

// owner says which session owner a command may address through --executor.
type owner int

const (
	anyOwner    owner = iota
	homeOwner         // docker or omitted; a remote executor is rejected
	remoteOwner       // an explicitly configured remote executor is required
)

// command declares one subcommand. Its flags are the only ones it registers,
// so help, parsing and the command reference all come from this table.
type command struct {
	name, operands, summary string
	flags                   []string
	owner                   owner
}

// commands lists every subcommand in help order. A command with an "id" flag
// requires a session UUID, except start, which creates one when omitted.
var commands = []command{
	{name: actionRun, operands: "--executor NAME (--task TEXT | --task-file FILE)",
		summary: "Submit a Codex task to a configured remote executor", owner: remoteOwner,
		flags: []string{"config", "executor", "operation", "harness", "repository", "project", "task", "task-file",
			"workspace-from", "checkpoint-digest", "revision"}},
	{name: home.ActionSessions, summary: "List sessions from home and configured remote executors",
		flags: []string{"config", "socket", "executor", "project"}},
	{name: home.ActionStatus, operands: "SESSION_UUID", summary: "Show one session's status",
		flags: []string{"config", "socket", "executor", "id"}},
	{name: home.ActionPull, operands: "SESSION_UUID --from NAME",
		summary: "Claim a stopped remote session and import it into home without starting an agent",
		flags:   []string{"config", "socket", "executor", "id", "operation", "from"}},
	{name: home.ActionResume, operands: "SESSION_UUID", owner: homeOwner,
		summary: "Start a new local execution of retained work and attach to it",
		flags:   []string{"config", "socket", "executor", "id", "operation", "detach"}},
	{name: home.ActionStop, operands: "SESSION_UUID", owner: homeOwner,
		summary: "Stop local work after its current turn and keep a checkpoint",
		flags:   []string{"config", "socket", "executor", "id", "operation"}},
	{name: home.ActionCancel, operands: "SESSION_UUID", summary: "Revoke and force-stop an execution",
		flags: []string{"config", "socket", "executor", "id", "operation"}},
	{name: home.ActionRecover, operands: "SESSION_UUID", summary: "Continue a journaled operation after an interruption",
		flags: []string{"config", "socket", "executor", "id"}},
	{name: actionServe, operands: "--config FILE", summary: "Run the home daemon",
		flags: []string{"config"}},
	{name: home.ActionStart, operands: "--workspace PATH", owner: homeOwner,
		summary: "Seed a managed Docker session from a stopped Git directory",
		flags:   []string{"config", "socket", "executor", "id", "operation", "workspace", "include-ignored"}},
	{name: home.ActionAttach, operands: "SESSION_UUID", owner: homeOwner,
		summary: "Attach this terminal to a home session; Ctrl-] detaches",
		flags:   []string{"config", "socket", "executor", "id"}},
	{name: home.ActionMove, operands: "SESSION_UUID --to NAME", owner: homeOwner,
		summary: "Move a home session to a configured placement after its current turn",
		flags:   []string{"config", "socket", "executor", "id", "operation", "to"}},
}

// flagHelp describes each flag once. A backquoted word names its value in help.
var flagHelp = map[string]string{
	"config":            "private global Sproozi configuration `FILE`",
	"socket":            "private home socket `PATH`; defaults to the configured home",
	"executor":          "configured executor `NAME`; docker selects home",
	"id":                "stable session `UUID`; may instead be the first argument",
	"operation":         "stable operation `UUID`; reuse it when retrying",
	"from":              "configured remote source `NAME`",
	"harness":           "approved native `harness`; defaults to the global configuration",
	"repository":        "approved repository `NAME`",
	"project":           "configured project `ID`",
	"task":              "task `TEXT`",
	"task-file":         "UTF-8 task `FILE`, at most 16 KiB",
	"workspace-from":    "retained source `SESSION_UUID` for fresh work at its recorded commit",
	"checkpoint-digest": "exact source checkpoint `SHA256` for --workspace-from",
	"revision":          "exact full recorded Git `COMMIT` for --workspace-from",
	"detach":            "return after resume without attaching",
	"workspace":         "stopped ordinary Git directory `PATH`",
	"include-ignored":   "ignored file `PATH` to carry on every visit; repeatable",
	"to":                "configured destination `NAME`",
}

func findCommand(name string) (command, bool) {
	for _, c := range commands {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

func (c command) accepts(flag string) bool {
	for _, name := range c.flags {
		if name == flag {
			return true
		}
	}
	return false
}

// inputs holds flag values that are checked or combined before they become options.
type inputs struct {
	config, project, workspaceFrom, checkpointDigest, revision, taskFile string
}

// flagSet registers only this command's flags, bound to q and in.
func (c command) flagSet(q *options, in *inputs) *flag.FlagSet {
	fs := flag.NewFlagSet("sproozi "+c.name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	values := map[string]*string{
		"config": &in.config, "socket": &q.socket, "executor": &q.executor, "id": &q.id,
		"operation": &q.operation, "from": &q.source, "harness": &q.kind, "repository": &q.repository,
		"project": &in.project, "task": &q.task, "task-file": &in.taskFile, "workspace-from": &in.workspaceFrom,
		"checkpoint-digest": &in.checkpointDigest, "revision": &in.revision, "workspace": &q.workspace,
		"to": &q.destination,
	}
	for _, name := range c.flags {
		switch name {
		case "detach":
			fs.BoolVar(&q.detach, name, false, flagHelp[name])
		case "include-ignored":
			fs.Func(name, flagHelp[name], func(v string) error {
				q.includeIgnored = append(q.includeIgnored, v)
				return workspace.ValidateIgnoredPaths(q.includeIgnored)
			})
		case "config":
			fs.StringVar(&in.config, name, c.defaultConfig(), flagHelp[name])
		default:
			fs.StringVar(values[name], name, "", flagHelp[name])
		}
	}
	return fs
}

// line is the command's synopsis without its optional flags.
func (c command) line() string {
	return strings.TrimSpace("sproozi " + c.name + " " + c.operands)
}

func (c command) usage(w io.Writer) {
	fs := c.flagSet(&options{}, &inputs{})
	_, _ = fmt.Fprintf(w, "Usage: %s [flags]\n\n%s.\n\nFlags:\n", c.line(), c.summary)
	fs.SetOutput(w)
	fs.PrintDefaults()
}

// defaultConfig is the platform configuration path; serve has none and
// requires an explicit --config.
func (c command) defaultConfig() string {
	if c.name == actionServe {
		return ""
	}
	path, _ := defaultConfigPath()
	return path
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "Usage: sproozi COMMAND [flags]\n\nCommands:")
	for _, c := range commands {
		_, _ = fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
	}
	_, _ = fmt.Fprintln(w, "\nRun \"sproozi COMMAND -h\" for a command's flags.")
}

func helpRequested(arg string) bool {
	return arg == "help" || strings.TrimLeft(arg, "-") == "h" || strings.TrimLeft(arg, "-") == "help"
}
