//nolint:goconst // Flag names read best as literals beside the table that declares them.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/work"
	"github.com/andrewmccall/sproozi/internal/workspace"
)

// usageError is a command line Sproozi cannot run as written. It exits 2.
type usageError struct{ message string }

func (e *usageError) Error() string { return e.message }

func usagef(format string, args ...any) error {
	return &usageError{message: fmt.Sprintf(format, args...)}
}

// errHelp asks for the command's help.
var errHelp = errors.New("help requested")

// options is one parsed command line.
type options struct {
	cmd                command
	verb, operand      string
	id, path           string
	harnessArgs        []string
	login              string
	root, configPath   string
	socket             string
	socketGiven        bool
	harness, model     string
	image, build       string
	providerURL        string
	providerCA         string
	credentialFile     string
	instructionsFile   string
	credentialStdin    bool
	skipCheck          bool
	signIn             bool
	token              bool
	chatgpt            bool
	noService          bool
	json               bool
	leaveRunning       bool
	checkProvider      bool
	detach, remote     bool
	local              bool
	context, namespace string
	task, repository   string
	project            project.ID
	operation          string
	includeIgnored     []string
	workspaceFrom      *work.RevisionSource
	port               uint16
	timeout            time.Duration
	fixed              home.Config
	out                io.Writer
}

func (q *options) action() string { return q.cmd.name }

// parse reads one command line, args[0] naming the command. It returns errHelp
// when the command's help was requested.
func parse(args []string) (options, error) {
	c, ok := findCommand(args[0])
	if !ok {
		return options{}, unknownCommand(args[0])
	}
	q := options{cmd: c}
	rest := args[1:]
	if c.harnessArgs {
		if i := slices.Index(rest, "--"); i >= 0 {
			rest, q.harnessArgs = rest[:i], slices.Clone(rest[i+1:])
		}
	}
	strs, bools := map[string]*string{}, map[string]*bool{}
	fs := flag.NewFlagSet("sproozi "+c.name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	for _, name := range append(slices.Clone(c.flags), c.hiddenFlags...) {
		spec := flagSpecs[name]
		switch {
		case name == "include-ignored":
			fs.Func(name, spec.help, func(v string) error {
				q.includeIgnored = append(q.includeIgnored, v)
				return workspace.ValidateIgnoredPaths(q.includeIgnored)
			})
		case spec.boolean():
			bools[name] = fs.Bool(name, false, spec.help)
			if spec.short != "" {
				fs.BoolVar(bools[name], spec.short, false, spec.help)
			}
		default:
			strs[name] = fs.String(name, "", spec.help)
		}
	}
	positionals, err := parseInterleaved(c, fs, rest)
	if err != nil {
		return options{cmd: c}, err
	}
	str := func(name string) string {
		if p := strs[name]; p != nil {
			return strings.TrimSpace(*p)
		}
		return ""
	}
	boolean := func(name string) bool { return bools[name] != nil && *bools[name] }
	fs.Visit(func(f *flag.Flag) { q.socketGiven = q.socketGiven || f.Name == "socket" })
	q.root, q.configPath, q.socket = str("root"), str("config"), str("socket")
	q.harness, q.model, q.image, q.build = str("harness"), str("model"), str("image"), str("build")
	q.providerURL, q.providerCA, q.credentialFile = str("provider-url"), str("provider-ca-file"), str("credential-file")
	q.instructionsFile, q.context, q.namespace = str("instructions-file"), str("context"), str("namespace")
	q.task, q.repository, q.project = str("task"), str("repository"), project.ID(str("project"))
	q.operation, q.id, q.login = str("operation"), str("id"), str("login")
	q.local = boolean("local")
	q.credentialStdin, q.skipCheck = boolean("credential-stdin"), boolean("skip-credential-check")
	q.signIn, q.token, q.chatgpt = boolean("sign-in"), boolean("token"), boolean("chatgpt")
	q.noService, q.json, q.leaveRunning = boolean("no-service"), boolean("json"), boolean("leave-running")
	q.checkProvider, q.detach = boolean("check-provider"), boolean("detach")
	q.remote = boolean("remote") || q.context != "" || q.namespace != ""
	if err = q.takePositionals(positionals); err != nil {
		return options{}, err
	}
	if err = q.validate(str); err != nil {
		return options{}, err
	}
	return q, nil
}

// parseInterleaved parses flags that may come before or after the operands.
func parseInterleaved(c command, fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, errHelp
			}
			return nil, flagError(c, err)
		}
		if fs.NArg() == 0 {
			return positionals, nil
		}
		positionals, args = append(positionals, fs.Arg(0)), fs.Args()[1:]
	}
}

// flagError rewrites the flag package's message for an unknown or malformed flag.
func flagError(c command, err error) error {
	if name, ok := strings.CutPrefix(err.Error(), "flag provided but not defined: -"); ok {
		name = strings.TrimPrefix(name, "-")
		if hint := suggest(name, c.flags); hint != "" {
			return usagef("%s does not accept --%s; did you mean --%s?", c.name, name, hint)
		}
		if name == "harness-args" || c.harnessArgs {
			return usagef("%s does not accept --%s; put arguments for the agent after --, and run \"sproozi help %s\" "+
				"for the flags", c.name, name, c.name)
		}
		return usagef("%s does not accept --%s; run \"sproozi help %s\" for its flags", c.name, name, c.name)
	}
	if m := invalidFlagValue.FindStringSubmatch(err.Error()); m != nil {
		return usagef("--%s %s: %s", m[2], m[1], strings.TrimPrefix(m[3], "workspace: "))
	}
	return usagef("%s: %v; run \"sproozi help %s\" for its flags", c.name,
		strings.ReplaceAll(err.Error(), " -", " --"), c.name)
}

// invalidFlagValue matches the flag package's message for a rejected value.
var invalidFlagValue = regexp.MustCompile(`^invalid (?:value|boolean value) ("[^"]*") for flag -([a-z-]+): (.*)$`)

func unknownCommand(name string) error {
	if hint := suggest(name, commandNames()); hint != "" {
		return usagef("unknown command %q; did you mean %q?", name, hint)
	}
	return usagef("unknown command %q; run \"sproozi help\" for the commands", name)
}

// takePositionals assigns the verb, operand, session ID or PATH.
func (q *options) takePositionals(positionals []string) error {
	c := q.cmd
	if len(c.verbs) > 0 {
		if len(positionals) == 0 {
			return usagef("%s needs one of %s; usage: %s", c.name, strings.Join(c.verbs, ", "), c.usageLine())
		}
		q.verb, positionals = positionals[0], positionals[1:]
		if !slices.Contains(c.verbs, q.verb) && !slices.Contains(c.hiddenVerbs, q.verb) {
			if hint := suggest(q.verb, c.verbs); hint != "" {
				return usagef("%s does not support %q; did you mean %q?", c.name, q.verb, hint)
			}
			return usagef("%s does not support %q; usage: %s", c.name, q.verb, c.usageLine())
		}
		if slices.Contains(c.operandVerbs, q.verb) {
			if len(positionals) == 0 {
				return usagef("%s %s needs a name; usage: %s", c.name, q.verb, c.usageLine())
			}
			q.operand, positionals = positionals[0], positionals[1:]
		}
	}
	switch {
	case c.session && len(positionals) > 0:
		if q.id != "" {
			return usagef("give the session ID once")
		}
		q.id, positionals = positionals[0], positionals[1:]
	case c.path && len(positionals) > 0:
		q.path, positionals = positionals[0], positionals[1:]
	}
	if len(positionals) > 0 {
		return usagef("%s: unexpected argument %q; usage: %s", c.name, positionals[0], c.usageLine())
	}
	if c.session && q.id == "" {
		return usagef("%s needs a session ID; usage: %s (\"sproozi ls\" lists them)", c.name, c.usageLine())
	}
	return nil
}

// validate checks flag values and combinations before anything is read or sent.
func (q *options) validate(str func(string) string) error {
	c := q.cmd
	if q.id != "" && !validIDPrefix(q.id) {
		return usagef("%q is not a session ID; give the ID, or its first characters, as \"sproozi ls\" shows it", q.id)
	}
	if q.operation != "" && !work.CanonicalID(q.operation) {
		return usagef("--operation %q is not a UUID", q.operation)
	}
	if q.harness != "" {
		if err := harness.NativeKind(q.harness).Validate(); err != nil {
			return usagef("--harness must be %s", harness.ManagedKindList())
		}
	}
	if q.login != "" {
		if err := harness.ValidateLogin(q.login); err != nil {
			return usagef("--login: %v", err)
		}
	}
	if q.project != "" && !project.ValidID(q.project) {
		return usagef("--project %q is not a valid project ID", q.project)
	}
	if port := str("port"); port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n < 1024 {
			return usagef("--port must be a port number from 1024 to 65535")
		}
		q.port = uint16(n)
	}
	if timeout := str("timeout"); timeout != "" {
		d, err := time.ParseDuration(timeout)
		if err != nil || d <= 0 {
			return usagef("--timeout must be a positive duration such as 20m")
		}
		q.timeout = d
	}
	if q.credentialStdin && q.credentialFile != "" {
		return usagef("choose --credential-file or --credential-stdin")
	}
	if err := q.validateCredentialChoice(); err != nil {
		return err
	}
	if c.name == cmdMove && q.local == q.remote {
		return usagef("move needs --local (to this machine) or --remote (to a cluster)")
	}
	if c.name == cmdRun {
		if err := q.validateRun(str); err != nil {
			return err
		}
	}
	for _, path := range []*string{&q.root, &q.configPath, &q.path, &q.credentialFile, &q.providerCA,
		&q.instructionsFile, &q.build} {
		if *path != "" {
			absolute, err := filepath.Abs(*path)
			if err != nil {
				return err
			}
			*path = absolute
		}
	}
	// On Windows --socket names a named pipe and is used as given.
	if q.socket != "" && !isWindows() {
		absolute, err := filepath.Abs(q.socket)
		if err != nil {
			return err
		}
		q.socket = absolute
	}
	return nil
}

// validateRun checks that a run is either local or remote, not a mix.
func (q *options) validateRun(str func(string) string) error {
	taskFile := str("task-file")
	source, digest, revision := str("workspace-from"), str("checkpoint-digest"), str("revision")
	remoteOnly := map[string]bool{"--task": q.task != "", "--task-file": taskFile != "",
		"--repository": q.repository != "", "--project": q.project != "", "--workspace-from": source != ""}
	if !q.remote {
		for _, name := range []string{"--task", "--task-file", "--repository", "--project", "--workspace-from"} {
			if remoteOnly[name] {
				return usagef("%s applies only to a --remote run", name)
			}
		}
		return nil
	}
	if q.login != "" {
		return usagef("--login applies to a session on this machine; a session moved to a cluster keeps its login")
	}
	switch {
	case q.path != "":
		return usagef("a --remote run works in the cluster's approved repository; give --repository NAME instead of PATH")
	case len(q.harnessArgs) > 0:
		return usagef("a --remote run does not take agent arguments after --")
	case q.detach || len(q.includeIgnored) > 0:
		return usagef("--detach and --include-ignored apply only to a run on this machine")
	case q.task != "" && taskFile != "":
		return usagef("choose --task or --task-file")
	case q.task == "" && taskFile == "":
		return usagef("a --remote run needs --task TEXT or --task-file FILE")
	}
	if taskFile != "" {
		text, err := readTaskFile(taskFile)
		if err != nil {
			return err
		}
		q.task = text
	}
	if source != "" || digest != "" || revision != "" {
		if source == "" || digest == "" || revision == "" {
			return usagef("--workspace-from needs --checkpoint-digest and --revision")
		}
		q.workspaceFrom = &work.RevisionSource{SessionID: source, Digest: digest, Revision: revision}
	}
	return nil
}

func readTaskFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	data, err := io.ReadAll(io.LimitReader(f, 16<<10+1))
	if err = errors.Join(err, f.Close()); err != nil {
		return "", err
	}
	if len(data) > 16<<10 {
		return "", usagef("--task-file is larger than 16 KiB")
	}
	return string(data), nil
}

// validIDPrefix accepts a full session UUID or the start of one.
func validIDPrefix(id string) bool {
	if len(id) == 0 || len(id) > 36 {
		return false
	}
	for _, r := range id {
		if !strings.ContainsRune("0123456789abcdef-", r) {
			return false
		}
	}
	return true
}

func isWindows() bool { return goos == "windows" }

// validateCredentialChoice allows one way for the gateway or the harness to
// authenticate (ADR 0047).
func (q *options) validateCredentialChoice() error {
	if btoi(q.signIn)+btoi(q.token)+btoi(q.chatgpt) > 1 {
		return usagef("choose one of --sign-in, --token and --chatgpt")
	}
	keyInput := q.credentialStdin || q.credentialFile != ""
	if q.chatgpt && keyInput {
		return usagef("--chatgpt signs in in the browser; leave out --credential-file and --credential-stdin")
	}
	if q.signIn && keyInput {
		return usagef("--sign-in stores no key; leave out --credential-file and --credential-stdin")
	}
	return nil
}
