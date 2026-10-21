// Command sproozi runs coding agents in isolated containers, and runs the
// background service that owns them on this machine.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
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
	err := run(args, stdout)
	if err == nil {
		return 0
	}
	_, _ = fmt.Fprintln(stderr, "error:", explain(err))
	var usage *usageError
	if errors.As(err, &usage) {
		return 2
	}
	return 1
}

func run(args []string, stdout io.Writer) error {
	switch {
	case args[0] == "-v" || args[0] == "--version":
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
	case cmdDoctor, cmdCredentials, cmdImages, cmdCertificates:
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
	defer func() { _ = term.Restore(fd, state) }()
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
	_, _ = fmt.Fprintln(os.Stderr, "Attached to the agent's terminal; Ctrl-] detaches")
	return home.AttachTerminal(run, socket, id, os.Stdin, os.Stdout, sizes)
}
