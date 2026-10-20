//nolint:goconst // Flag names read best as literals in the command table.
package main

import (
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/workspace"
)

const (
	actionServe        = "serve"
	actionVersion      = "version"
	actionService      = "service"
	actionDoctor       = "doctor"
	actionCredentials  = "credentials"
	actionImages       = "images"
	actionCertificates = "certificates"
	configFlag         = "config"
)

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
	// verbs, when set, are the accepted first positional arguments. A verb in
	// operandVerbs also takes one further positional operand.
	verbs, operandVerbs []string
}

// commands lists every subcommand in help order. A command with an "id" flag
// requires a session UUID, except start, which creates one when omitted.
var commands = []command{
	{name: actionInit, operands: "[--local]", summary: "Create a private configuration directory, or provision local Docker sessions with --local",
		flags: []string{"directory", "local", "root", "harness", "model", "instructions-file", "image", "build", "provider-url", "provider-ca-file",
			"credential-file", "credential-stdin", "skip-credential-check", "port", "no-service", "json", "timeout"}},
	{name: actionService, operands: "status|start|stop", summary: "Show, start or stop the local home service",
		flags: []string{"root", "json", "leave-running"}, verbs: []string{"status", "start", "stop"}},
	{name: actionDoctor, summary: "Check the local installation without changing it",
		flags: []string{"root", "json", "check-provider"}},
	{name: actionCredentials, operands: "add openai|anthropic", summary: "Store or replace a gateway-owned provider credential",
		flags: []string{"root", "credential-file", "credential-stdin", "skip-credential-check"}, verbs: []string{"add"}, operandVerbs: []string{"add"}},
	{name: actionImages, operands: "status|prepare", summary: "Show or prepare the native worker image",
		flags: []string{"root", "image", "build", "json"}, verbs: []string{"status", "prepare"}},
	{name: actionCertificates, operands: "status|renew|rotate-ca|replace-ca", summary: "Show or maintain the local gateway certificate authority",
		flags: []string{"root", "json"}, verbs: []string{"status", "renew", "rotate-ca", "replace-ca"}},
	{name: actionVersion, summary: "Show the installed product version"},
	{name: actionRun, operands: "--executor NAME (--task TEXT | --task-file FILE)",
		summary: "Submit a task to a configured remote executor", owner: remoteOwner,
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
		summary: "Start a new local execution of a stopped session and attach to it",
		flags:   []string{"config", "socket", "executor", "id", "operation", "detach"}},
	{name: home.ActionStop, operands: "SESSION_UUID", owner: homeOwner,
		summary: "Stop local work after its current turn and keep a checkpoint",
		flags:   []string{"config", "socket", "executor", "id", "operation"}},
	{name: home.ActionCancel, operands: "SESSION_UUID", summary: "Revoke and force-stop a session's current execution",
		flags: []string{"config", "socket", "executor", "id", "operation"}},
	{name: home.ActionRecover, operands: "SESSION_UUID", summary: "Continue a journaled operation after an interruption",
		flags: []string{"config", "socket", "executor", "id"}},
	{name: actionServe, summary: "Run the home daemon",
		flags: []string{"config"}},
	{name: home.ActionStart, operands: "--workspace PATH", owner: homeOwner,
		summary: "Start a managed Docker session from a stopped Git directory and attach to it",
		flags:   []string{"config", "socket", "executor", "id", "operation", "workspace", "harness", "include-ignored", "detach"}},
	{name: home.ActionAttach, operands: "SESSION_UUID", owner: homeOwner,
		summary: "Attach this terminal to a home session; Ctrl-] detaches",
		flags:   []string{"config", "socket", "executor", "id"}},
	{name: home.ActionMove, operands: "SESSION_UUID --to NAME", owner: homeOwner,
		summary: "Move a home session to a configured placement after its current turn",
		flags:   []string{"config", "socket", "executor", "id", "operation", "to"}},
}

// flagHelp describes each flag once. A backquoted word names its value in help.
var flagHelp = map[string]string{
	"directory":             "private configuration directory `PATH`",
	"config":                "private global Sproozi configuration `FILE`",
	"socket":                "private home socket `PATH`; defaults to the configured home",
	"executor":              "executor `NAME`: docker (home), or a name under \"remote\" or \"workSources\"",
	"id":                    "stable session `UUID`; may instead be a positional argument",
	"operation":             "stable operation `UUID`; reuse it when retrying",
	"from":                  "remote executor `NAME` to pull from, configured under \"remote\" or \"workSources\"",
	"harness":               "native harness `NAME`, codex or claude-code; defaults to the primary configured harness",
	"repository":            "approved repository `NAME`",
	"project":               "configured project `ID`",
	"task":                  "task `TEXT`",
	"task-file":             "UTF-8 task `FILE`, at most 16 KiB",
	"workspace-from":        "retained source `SESSION_UUID` for fresh work at its recorded commit",
	"checkpoint-digest":     "exact source checkpoint `SHA256` for --workspace-from",
	"revision":              "exact full recorded Git `COMMIT` for --workspace-from",
	"detach":                "return without attaching to the native terminal",
	"workspace":             "stopped ordinary Git directory `PATH`",
	"include-ignored":       "ignored file `PATH` to carry on every visit; repeatable",
	"to":                    "placement `NAME`: the value of local.name (for example local) or a name under \"remote\"",
	"local":                 "provision local Docker sessions: images, network, TLS, credential, configuration and service",
	"root":                  "private installation root `PATH`; defaults to the recorded root or ~/sproozi",
	"model":                 "provider `MODEL` for the selected harness",
	"image":                 "approved native image `REFERENCE`, digest-pinned, or a local sha256 content ID",
	"build":                 "build the native image from a Sproozi source `DIRECTORY` with Docker",
	"provider-url":          "provider base `URL`; defaults to the harness's provider",
	"provider-ca-file":      "private CA bundle `FILE` for a private provider endpoint",
	"credential-file":       "owner-only `FILE` holding the provider credential",
	"credential-stdin":      "read the provider credential from standard input",
	"skip-credential-check": "do not ask the provider to validate the credential",
	"port":                  "first gateway `PORT` to try",
	"no-service":            "write configuration without starting the home service",
	"json":                  "print a machine-readable result",
	"timeout":               "overall `DURATION` limit",
	"leave-running":         "stop the service while active sessions keep running within their deadlines",
	"check-provider":        "also ask each provider to validate its credential; lists models only",
}

// runFlagHelp replaces flagHelp where run takes a narrower value.
var runFlagHelp = map[string]string{
	"executor": "remote executor `NAME` configured under \"remote\" or \"workSources\"; run does not accept docker",
}

// help describes one of this command's flags.
func (c command) help(name string) string {
	if text, ok := runFlagHelp[name]; ok && c.name == actionRun {
		return text
	}
	return flagHelp[name]
}

func findCommand(name string) (command, bool) {
	for _, c := range commands {
		if c.name == name {
			return c, true
		}
	}
	return command{}, false
}

func (c command) accepts(name string) bool {
	return slices.Contains(c.flags, name)
}

// inputs holds flag values that are checked or combined before they become options.
type inputs struct {
	config, project, workspaceFrom, checkpointDigest, revision, taskFile string
	port, timeout                                                        string
}

// boolFlags are switches; every other flag takes a value.
var boolFlags = []string{"detach", "local", "credential-stdin", "skip-credential-check", "no-service", "json", "leave-running", "check-provider"}

// flagSet registers only this command's flags, bound to q and in.
func (c command) flagSet(q *options, in *inputs) *flag.FlagSet {
	fs := flag.NewFlagSet("sproozi "+c.name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	values := map[string]*string{
		"socket": &q.socket, "executor": &q.executor, "id": &q.id,
		"operation": &q.operation, "from": &q.source, "harness": &q.kind, "repository": &q.repository,
		"project": &in.project, "task": &q.task, "task-file": &in.taskFile, "workspace-from": &in.workspaceFrom,
		"checkpoint-digest": &in.checkpointDigest, "revision": &in.revision, "workspace": &q.workspace,
		"to":        &q.destination,
		"directory": &q.directory,
		"root":      &q.setup.root, "model": &q.setup.model, "image": &q.setup.image, "build": &q.setup.build,
		"provider-url": &q.setup.providerURL, "instructions-file": &q.setup.instructionsFile, "provider-ca-file": &q.setup.providerCA, "credential-file": &q.setup.credentialFile,
		"port": &in.port, "timeout": &in.timeout,
	}
	switches := map[string]*bool{
		"detach": &q.detach, "local": &q.setup.enabled, "credential-stdin": &q.setup.credentialStdin,
		"skip-credential-check": &q.setup.skipCheck, "no-service": &q.setup.noService, "json": &q.setup.json,
		"leave-running": &q.setup.leaveRunning, "check-provider": &q.setup.checkProvider,
	}
	for _, name := range c.flags {
		switch {
		case slices.Contains(boolFlags, name):
			fs.BoolVar(switches[name], name, false, c.help(name))
		case name == "include-ignored":
			fs.Func(name, c.help(name), func(v string) error {
				q.includeIgnored = append(q.includeIgnored, v)
				return workspace.ValidateIgnoredPaths(q.includeIgnored)
			})
		case name == configFlag:
			fs.StringVar(&in.config, name, c.defaultConfig(), c.help(name))
		default:
			fs.StringVar(values[name], name, "", c.help(name))
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
	printFlags(w, fs)
}

// printFlags lists flags as the usage line spells them, with two dashes.
// flag.PrintDefaults would print one.
func printFlags(w io.Writer, fs *flag.FlagSet) {
	fs.VisitAll(func(f *flag.Flag) {
		name, usage := flag.UnquoteUsage(f)
		line := "  --" + f.Name
		if name != "" {
			line += " " + name
		}
		line += "\n    \t" + strings.ReplaceAll(usage, "\n", "\n    \t")
		if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "0" {
			line += fmt.Sprintf(" (default %q)", f.DefValue)
		}
		_, _ = fmt.Fprintln(w, line)
	})
}

// defaultConfig is the platform configuration path that init creates.
func (c command) defaultConfig() string {
	path, _ := defaultConfigPath()
	return path
}

func usage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "Usage: sproozi COMMAND [flags]\n\nCommands:")
	for _, c := range commands {
		_, _ = fmt.Fprintf(w, "  %-9s %s\n", c.name, c.summary)
	}
	_, _ = fmt.Fprintln(w, "\nHome is the home daemon: the \"sproozi serve\" process on your own machine. It owns\n"+
		"local Docker placements, their gateway and accounting, and the sessions you have pulled.\n"+
		"Run \"sproozi init\" to create a starting configuration.\n"+
		"Run \"sproozi COMMAND -h\" for a command's flags.")
}

func helpRequested(arg string) bool {
	return slices.Contains([]string{"help", "-h", "-help", "--help"}, arg)
}
