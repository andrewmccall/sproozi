// Command sproozi runs coding agents in isolated containers, and runs the
// background service that owns them on this machine.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"slices"
	"strings"
	"syscall"

	"github.com/andrewmccall/sproozi/internal/session/home"
	"golang.org/x/term"
)

// goos names the platform; tests may not change it.
var goos = runtime.GOOS

func main() {
	os.Exit(cli(os.Args[1:], os.Stdout, os.Stderr))
}

// cli runs one command line and returns its exit status: 0 on success, 2 for
// a command line that cannot run as written, 1 for any other failure.
func cli(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printTopUsage(stderr)
		return 2
	}
	args = commandFirst(args)
	err := run(args, stdout)
	if err == nil {
		return 0
	}
	code := 1
	var usage *usageError
	if errors.As(err, &usage) {
		code = 2
	}
	if wantsJSON(args) {
		message, next, _ := strings.Cut(explain(err), "; ")
		_ = json.NewEncoder(stderr).Encode(struct {
			Error    string `json:"error"`
			Next     string `json:"next,omitempty"`
			ExitCode int    `json:"exitCode"`
		}{message, strings.TrimPrefix(next, "next: "), code})
		return code
	}
	_, _ = fmt.Fprintln(stderr, "error:", explain(err))
	return code
}

// commandFirst moves flags given before the command, as in "sproozi --json
// ls", after it, where the command parses them.
func commandFirst(args []string) []string {
	if !strings.HasPrefix(args[0], "-") || helpRequested(args[0]) || versionRequested(args[0]) {
		return args
	}
	names := commandNames()
	for i, arg := range args {
		if arg == "--" {
			break
		}
		if slices.Contains(names, arg) {
			return append(append([]string{arg}, args[:i]...), args[i+1:]...)
		}
	}
	return args
}

func versionRequested(arg string) bool { return arg == "-v" || arg == "--version" }

// wantsJSON reports --json among Sproozi's own arguments.
func wantsJSON(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "--json" || arg == "-json" {
			return true
		}
	}
	return false
}

func run(args []string, stdout io.Writer) error {
	switch {
	case versionRequested(args[0]):
		args = append([]string{cmdVersion}, args[1:]...)
	case helpRequested(args[0]):
		printTopUsage(stdout)
		return nil
	case args[0] == cmdHelp:
		return help(args[1:], stdout)
	}
	q, err := parse(args)
	if errors.Is(err, errHelp) {
		q.cmd.printUsage(stdout)
		return nil
	}
	if err != nil {
		return err
	}
	q.out = stdout
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return q.dispatch(ctx)
}

// help prints the top-level help or one command's.
func help(topics []string, out io.Writer) error {
	switch len(topics) {
	case 0:
		printTopUsage(out)
		return nil
	case 1:
		if topics[0] == "help" {
			// help is handled before the command table; describe it here
			// rather than suggest the name the user just typed.
			_, _ = fmt.Fprintln(out, "Usage: sproozi help [command]\n\nShow all commands, or one command's usage and flags.")
			return nil
		}
		c, ok := findCommand(topics[0])
		if !ok {
			return unknownCommand(topics[0])
		}
		c.printUsage(out)
		return nil
	}
	return usagef("help takes one command name")
}

func (q *options) dispatch(ctx context.Context) error {
	switch q.action() {
	case cmdVersion:
		return q.version()
	case cmdInit:
		return q.initLocal(ctx)
	case cmdImages:
		if q.verb == verbBuild {
			return q.buildImages(ctx)
		}
		return q.maintain(ctx)
	case cmdDoctor, cmdCredentials, cmdCertificates:
		return q.maintain(ctx)
	case cmdService:
		if q.verb == verbRun {
			return q.serve(ctx)
		}
		return q.maintain(ctx)
	case cmdSessions:
		return q.sessions(ctx)
	case cmdRun:
		if q.remote {
			return q.submitRemote(ctx)
		}
	}
	return q.session(ctx)
}

// serve runs the service in the foreground for "service run".
func (q *options) serve(ctx context.Context) error {
	path, err := q.config()
	if err != nil {
		return err
	}
	fixed, err := readConfig(path)
	if err != nil {
		return err
	}
	if err = serveReady(path, fixed); err != nil {
		return err
	}
	// Open saves the local placement settings, so check Docker first: a failed
	// check must not record settings the operator is about to correct.
	if err = home.CheckDocker(ctx, fixed); err != nil {
		return err
	}
	// A port in use would otherwise fail only after Open had created a new
	// state directory. A service already serving it is named as such.
	if err = home.CheckListen(ctx, fixed); err != nil {
		return err
	}
	service, err := home.Open(fixed, os.Stderr)
	if err != nil {
		return err
	}
	defer func() { _ = service.Close() }()
	return service.Serve(ctx)
}

// errNoTerminal explains why attach cannot run: the agent's terminal needs an
// interactive terminal on standard input.
var errNoTerminal = errors.New("attach needs an interactive terminal, and standard input is not one; " +
	"run it from a terminal, where Ctrl-] detaches")

func attach(ctx context.Context, socket, id string) error {
	sizes := make(chan home.TerminalSize, 1)
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return errNoTerminal
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return err
	}
	restored := false
	restore := func() {
		if !restored {
			restored = true
			_ = term.Restore(fd, state)
		}
	}
	defer restore()
	var last home.TerminalSize
	update := func() {
		columns, rows, err := terminalSize(fd)
		if err == nil && columns > 0 && rows > 0 && columns <= 65535 && rows <= 65535 {
			size := home.TerminalSize{Rows: uint16(rows), Columns: uint16(columns)}
			if size == last {
				return
			}
			select {
			case sizes <- size:
				last = size
			default:
			}
		}
	}
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	stopResize := watchResize(run, update)
	defer stopResize()
	err = home.AttachTerminalNotify(run, socket, id, os.Stdin, os.Stdout, sizes, func() {
		_, _ = fmt.Fprint(os.Stderr, "Attached to the agent's terminal; Ctrl-] detaches\r\n")
	})
	restore()
	if err == nil {
		// The agent is still running, so its screen modes and status bar are
		// still in this terminal. Reset them, then name the session and the
		// next commands below the cursor.
		_, _ = fmt.Fprint(os.Stdout, detachReset)
		_, _ = fmt.Fprintln(os.Stderr, detachedLine(id))
	}
	return err
}

// detachReset returns a terminal the agent drew on to a plain shell: default
// colours, visible cursor, no bracketed paste, mouse reporting or enhanced
// keyboard, and nothing left below the cursor to show through the hint.
const detachReset = "\x1b[0m\x1b[?25h\x1b[?2004l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l\x1b[<u\r\n\x1b[J"

// detachedLine names the session and the next commands after its terminal closes.
func detachedLine(id string) string {
	short := shortID(id)
	return fmt.Sprintf("Left session %s. Back to it: sproozi attach %s   Stop it: sproozi stop %s   List: sproozi ls",
		id, short, short)
}
