//nolint:goconst // Flag names and value names read best as literals in the command table.
package main

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// Command names.
const (
	cmdRun          = "run"
	cmdSessions     = "sessions"
	cmdStatus       = "status"
	cmdAttach       = "attach"
	cmdStop         = "stop"
	cmdResume       = "resume"
	cmdCancel       = "cancel"
	cmdRemove       = "rm"
	cmdMove         = "move"
	cmdPull         = "pull"
	cmdRecover      = "recover"
	cmdInit         = "init"
	cmdDoctor       = "doctor"
	cmdService      = "service"
	cmdCredentials  = "credentials"
	cmdImages       = "images"
	cmdCertificates = "certificates"
	cmdVersion      = "version"
	cmdHelp         = "help"
)

// Verbs of the setup commands.
const (
	verbStatus    = "status"
	verbStart     = "start"
	verbStop      = "stop"
	verbRun       = "run"
	verbSet       = "set"
	verbList      = "list"
	verbPrepare   = "prepare"
	verbRenew     = "renew"
	verbRotateCA  = "rotate-ca"
	verbReplaceCA = "replace-ca"
)

// group places a command in the top-level help; hidden commands are not listed.
type group int

const (
	hidden group = iota
	groupSessions
	groupRemote
	groupSetup
)

// command declares one subcommand. Its flags are the only ones it accepts, so
// help, parsing and the command reference all come from this table.
type command struct {
	name    string
	aliases []string
	// operands follow the name in the usage line.
	operands string
	summary  string
	// about is the longer description command help prints.
	about string
	group group
	// flags lists visible flags in help order; hiddenFlags are accepted but
	// not shown, for scripts and the administrator's hand-written setup.
	flags, hiddenFlags []string
	// verbs are the accepted first operands; hiddenVerbs are accepted but not
	// shown. A verb in operandVerbs takes one further operand.
	verbs, hiddenVerbs, operandVerbs []string
	// session commands take one session ID; path commands take an optional PATH.
	session, path bool
	// harnessArgs commands pass the arguments after "--" to the agent.
	harnessArgs bool
}

// locationFlags select a hand-written configuration or a control socket
// instead of an installation. They are hidden: --root is the location flag.
var locationFlags = []string{"config", "socket"}

var commands = []command{
	{name: cmdRun, operands: "[PATH] [-- AGENT ARGS]", group: groupSessions, path: true, harnessArgs: true,
		summary: "Start an agent on a Git repository (default: the current directory)",
		about: "Start an agent on a copy of the Git repository at PATH, in an isolated container, and connect\n" +
			"your terminal to it. PATH defaults to the current directory. Arguments after -- go to the\n" +
			"agent program unchanged, for example: sproozi run -- --model gpt-5.\n\n" +
			deadlineHelp + "\n\n" +
			"With --remote, submit a task to run in a Kubernetes cluster instead.",
		flags: []string{"harness", "detach", "include-ignored", "remote", "context", "namespace", "task", "task-file",
			"repository", "project", "json", "root"},
		hiddenFlags: append([]string{"id", "operation", "workspace-from", "checkpoint-digest", "revision"},
			locationFlags...)},
	{name: cmdSessions, aliases: []string{"ls"}, group: groupSessions, summary: "List sessions",
		about: "List the sessions on this machine and in the cluster. Sessions on this machine are listed\n" +
			"only while the Sproozi service runs; this command never starts it.",
		flags: []string{"remote", "context", "namespace", "project", "json", "root"}, hiddenFlags: locationFlags},
	{name: cmdStatus, operands: "ID", group: groupSessions, session: true, summary: "Show a session",
		flags: []string{"remote", "context", "namespace", "json", "root"}, hiddenFlags: sessionPlumbing(false)},
	{name: cmdAttach, operands: "ID", group: groupSessions, session: true,
		summary: "Connect your terminal to a running session; Ctrl-] detaches",
		flags:   []string{"root"}, hiddenFlags: sessionPlumbing(false)},
	{name: cmdStop, operands: "ID", group: groupSessions, session: true,
		summary: "Stop a session after the agent's current turn and keep its work",
		flags:   []string{"json", "root"}, hiddenFlags: sessionPlumbing(true)},
	{name: cmdResume, operands: "ID [-- AGENT ARGS]", group: groupSessions, session: true, harnessArgs: true,
		summary: "Continue a stopped session where it left off",
		about: "Continue a stopped session where it left off and connect your terminal to it (-d skips that).\n" +
			"Arguments after -- go to the agent program unchanged.\n\n" + deadlineHelp,
		flags: []string{"detach", "json", "root"}, hiddenFlags: sessionPlumbing(true)},
	{name: cmdCancel, operands: "ID", group: groupSessions, session: true,
		summary: "Stop a session now, discarding the agent's current turn",
		flags:   []string{"remote", "context", "namespace", "json", "root"}, hiddenFlags: sessionPlumbing(true)},
	{name: cmdRemove, operands: "ID", group: groupSessions, session: true,
		summary: "Delete a stopped or cancelled session and its saved work",
		flags:   []string{"json", "root"}, hiddenFlags: sessionPlumbing(false)},
	{name: cmdMove, operands: "ID --local|--remote", group: groupRemote, session: true,
		summary: "Move a running session between this machine and a cluster",
		about: "Move a running session after the agent's current turn: --local brings it to this machine,\n" +
			"--remote sends it to a cluster.",
		flags: []string{"local", "remote", "context", "namespace", "json", "root"}, hiddenFlags: sessionPlumbing(true)},
	{name: cmdPull, operands: "ID", group: groupRemote, session: true,
		summary: "Bring a stopped session from a cluster to this machine",
		flags:   []string{"context", "namespace", "json", "root"}, hiddenFlags: sessionPlumbing(true)},
	{name: cmdRecover, operands: "ID", session: true,
		summary: "Finish an operation that was interrupted",
		flags:   []string{"remote", "context", "namespace", "json", "root"}, hiddenFlags: sessionPlumbing(false)},
	{name: cmdInit, group: groupSetup, summary: "Set up this machine to run agents in Docker",
		about: "Set up Sproozi on this machine: the agent image, a private Docker network, certificates,\n" +
			"your provider key and the background service. Run it again to add another agent with\n" +
			"--harness, or to repair the installation.",
		flags: []string{"harness", "model", "credential-file", "credential-stdin", "skip-credential-check",
			"token", "chatgpt", "sign-in",
			"provider-url", "provider-ca-file", "instructions-file", "image", "build", "port", "no-service",
			"timeout", "json", "root"}},
	{name: cmdDoctor, group: groupSetup, summary: "Check the installation without changing it",
		flags: []string{"check-provider", "json", "root"}},
	{name: cmdService, operands: "status|start|stop", group: groupSetup,
		summary: "Show, start or stop the background service",
		verbs:   []string{verbStatus, verbStart, verbStop}, hiddenVerbs: []string{verbRun},
		flags: []string{"leave-running", "json", "root"}, hiddenFlags: []string{"config"}},
	{name: cmdCredentials, operands: "set AGENT|PROVIDER | list", group: groupSetup,
		summary: "Set or list provider keys",
		about: "Store or replace the provider key an agent uses (codex or openai, claude-code or anthropic),\n" +
			"or list the stored keys. Keys stay outside the agent's container and are never printed.",
		verbs: []string{verbSet, verbList}, operandVerbs: []string{verbSet},
		flags: []string{"credential-file", "credential-stdin", "skip-credential-check", "json", "root"}},
	{name: cmdImages, operands: "status|prepare", group: groupSetup, summary: "Show or prepare the agent image",
		verbs: []string{verbStatus, verbPrepare}, flags: []string{"image", "build", "json", "root"}},
	{name: cmdCertificates, operands: "status|renew|rotate-ca|replace-ca", group: groupSetup,
		summary: "Show or maintain this machine's certificate authority",
		verbs:   []string{verbStatus, verbRenew, verbRotateCA, verbReplaceCA}, flags: []string{"json", "root"}},
	{name: cmdVersion, group: groupSetup, summary: "Show the version", flags: []string{"json"}},
}

// deadlineHelp says how long a session runs and where that is set.
const deadlineHelp = "Each run or resume lasts until its deadline: 30 minutes by default. Change it with\n" +
	"\"maxDuration\" in config.json in the installation directory, then restart the service.\n" +
	"\"sproozi stop\" before then keeps the work; \"sproozi resume\" starts a new deadline."

// sessionPlumbing are the hidden flags of a session command: the full ID,
// the retry operation and the location flags.
func sessionPlumbing(operation bool) []string {
	flags := []string{"id"}
	if operation {
		flags = append(flags, "operation")
	}
	return append(flags, locationFlags...)
}

// flagSpec describes a flag once: its value name, if it takes one, and help.
type flagSpec struct {
	value, help, short string
}

func (f flagSpec) boolean() bool { return f.value == "" }

var flagSpecs = map[string]flagSpec{
	"harness": {value: "NAME",
		help: "agent to run: " + harness.ManagedKindList() + " (default: the first one set up)"},
	"detach":                {short: "d", help: "start without connecting your terminal"},
	"include-ignored":       {value: "PATH", help: "also copy this Git-ignored file into the session; repeatable"},
	"remote":                {help: "run the task in a Kubernetes cluster instead of on this machine"},
	"context":               {value: "NAME", help: "kubeconfig context or file for the cluster (default: sproozi)"},
	"namespace":             {value: "NS", help: "namespace of Sproozi in that cluster"},
	"task":                  {value: "TEXT", help: "task for a --remote run"},
	"task-file":             {value: "FILE", help: "task for a --remote run, from a UTF-8 file of at most 16 KiB"},
	"repository":            {value: "NAME", help: "approved repository for a --remote run"},
	"project":               {value: "ID", help: "approved project for a --remote run"},
	"json":                  {help: "print JSON instead of text"},
	"root":                  {value: "PATH", help: "installation directory (default: $SPROOZI_ROOT or ~/.sproozi)"},
	"local":                 {help: "move it to this machine"},
	"model":                 {value: "MODEL", help: "model the agent requests (default: the agent's own default)"},
	"credential-file":       {value: "FILE", help: "read the provider key from a file only you can read"},
	"credential-stdin":      {help: "read the provider key from standard input"},
	"skip-credential-check": {help: "do not ask the provider to check the key"},
	"sign-in":               {help: "let the agent sign in to its own provider account instead of storing a key"},
	"token":                 {help: "the stored credential is a sign-in token, such as from setup-token, not an API key"},
	"chatgpt":               {help: "sign the gateway in to your ChatGPT plan in the browser, for an OpenAI agent"},
	"provider-url":          {value: "URL", help: "address of a private provider endpoint (default: the public one)"},
	"provider-ca-file":      {value: "FILE", help: "PEM CA bundle for a private provider endpoint"},
	"instructions-file":     {value: "FILE", help: "approved instructions for every session, at most 16 KiB (first run)"},
	"image":                 {value: "REF", help: "agent image as REPOSITORY@sha256:DIGEST or a local sha256: ID"},
	"build":                 {value: "DIR", help: "build the agent image from a Sproozi source directory"},
	"port": {value: "PORT",
		help: "first port to try for this agent's gateway (default 18443, or the next free one)"},
	"no-service":     {help: "write the configuration without starting the service"},
	"timeout":        {value: "DURATION", help: "give up after this long (default: 30m)"},
	"check-provider": {help: "also ask each provider to check its key"},
	"leave-running":  {help: "stop the service even while sessions run; they run on until their deadline"},
	// Hidden plumbing for scripts and hand-written administrator setups.
	"id":                {value: "UUID", help: "session ID"},
	"operation":         {value: "UUID", help: "operation ID to repeat a request whose outcome is unknown"},
	"workspace-from":    {value: "UUID", help: "retained session to start a --remote run from"},
	"checkpoint-digest": {value: "SHA256", help: "checkpoint of that session"},
	"revision":          {value: "COMMIT", help: "commit of that session"},
	"config":            {value: "FILE", help: "hand-written configuration file instead of an installation"},
	"socket":            {value: "PATH", help: "service control socket"},
}

// commandFlagHelp replaces a flag's help where one command uses it differently.
var commandFlagHelp = map[string]map[string]string{
	cmdSessions: {"project": "list only this project's sessions",
		"remote":  "list only the cluster's sessions (the sproozi context unless --context)",
		"context": "list only this cluster's sessions: a kubeconfig context or file"},
	cmdStatus: {"remote": "show a session in the cluster (the sproozi context unless --context)",
		"context": "show a session in this cluster: a kubeconfig context or file"},
	cmdCancel: {"remote": "cancel a session in the cluster (the sproozi context unless --context)",
		"context": "cancel a session in this cluster: a kubeconfig context or file"},
	cmdRecover: {"remote": "recover a session in the cluster (the sproozi context unless --context)",
		"context": "recover a session in this cluster: a kubeconfig context or file"},
	cmdPull: {"context": "cluster to pull from, the first time: a kubeconfig context or file"},
	cmdMove: {"remote": "move it to a cluster"},
}

// flagHelp is a flag's help for this command.
func (c command) flagHelp(name string) string {
	if text, ok := commandFlagHelp[c.name][name]; ok {
		return text
	}
	return flagSpecs[name].help
}

func findCommand(name string) (command, bool) {
	for _, c := range commands {
		if c.name == name || slices.Contains(c.aliases, name) {
			return c, true
		}
	}
	return command{}, false
}

func (c command) usageLine() string {
	line := "sproozi " + c.name
	if c.operands != "" {
		line += " " + c.operands
	}
	if len(c.flags) > 0 {
		line += " [flags]"
	}
	return line
}

// flagLabel is how help spells a flag, with its short form and value name.
func flagLabel(name string) string {
	spec := flagSpecs[name]
	label := "    --" + name
	if spec.short != "" {
		label = "-" + spec.short + ", --" + name
	}
	if !spec.boolean() {
		label += " " + spec.value
	}
	return label
}

// printUsage writes a command's help: usage, description and one line per flag.
func (c command) printUsage(w io.Writer) {
	_, _ = fmt.Fprintf(w, "Usage: %s\n\n", c.usageLine())
	about := c.about
	if about == "" {
		about = c.summary + "."
	}
	_, _ = fmt.Fprintln(w, about)
	if len(c.aliases) > 0 {
		_, _ = fmt.Fprintf(w, "\nAlso: sproozi %s\n", strings.Join(c.aliases, ", sproozi "))
	}
	if len(c.flags) == 0 {
		return
	}
	width := 0
	for _, name := range c.flags {
		width = max(width, len(flagLabel(name)))
	}
	_, _ = fmt.Fprintln(w, "\nFlags:")
	for _, name := range c.flags {
		_, _ = fmt.Fprintf(w, "  %-*s  %s\n", width, flagLabel(name), c.flagHelp(name))
	}
}

// printTopUsage writes the grouped command list.
func printTopUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, "Sproozi runs coding agents (Codex, Claude Code) in isolated containers. Your provider\n"+
		"keys stay outside the container, and every request the agent makes is checked.\n\n"+
		"Usage: sproozi COMMAND [ARGS] [flags]")
	width := 0
	for _, c := range commands {
		if c.group != hidden {
			width = max(width, len(commandLabel(c)))
		}
	}
	for _, g := range []struct {
		group group
		title string
	}{{groupSessions, "Sessions"}, {groupRemote, "Remote"}, {groupSetup, "Setup"}} {
		_, _ = fmt.Fprintf(w, "\n%s:\n", g.title)
		for _, c := range commands {
			if c.group == g.group {
				_, _ = fmt.Fprintf(w, "  %-*s  %s\n", width, commandLabel(c), c.summary)
			}
		}
	}
	_, _ = fmt.Fprintln(w, "\nStart with \"sproozi init\", then \"sproozi run\" in a Git repository.\n"+
		"Run \"sproozi help COMMAND\" for a command's flags.")
}

func commandLabel(c command) string {
	return strings.Join(append([]string{c.name}, c.aliases...), ", ")
}

func helpRequested(arg string) bool {
	return slices.Contains([]string{"-h", "-help", "--help"}, arg)
}

// suggest returns the candidate closest to name when it is close enough to
// be a likely misspelling, otherwise "".
func suggest(name string, candidates []string) string {
	for _, candidate := range candidates {
		if len(name) >= 3 && strings.HasPrefix(candidate, name) {
			return candidate
		}
	}
	best, bestDistance := "", -1
	for _, candidate := range candidates {
		if d := distance(name, candidate); bestDistance < 0 || d < bestDistance {
			best, bestDistance = candidate, d
		}
	}
	allowed := 1
	if len(name) > 4 {
		allowed = 2
	}
	if bestDistance < 0 || bestDistance > allowed {
		return ""
	}
	return best
}

// distance is the optimal string alignment distance: insertions, deletions,
// substitutions and swaps of adjacent letters each count once.
func distance(a, b string) int {
	d := make([][]int, len(a)+1)
	for i := range d {
		d[i] = make([]int, len(b)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(a)][len(b)]
}

// commandNames lists every name and alias a user may type.
func commandNames() []string {
	names := []string{cmdHelp}
	for _, c := range commands {
		names = append(names, c.name)
		names = append(names, c.aliases...)
	}
	return names
}
