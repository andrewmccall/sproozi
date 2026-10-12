// Command session owns the trusted home daemon and its local terminal client.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/andrewmccall/sproozi/internal/session/home"
	"github.com/andrewmccall/sproozi/internal/workspace"
	"github.com/google/uuid"
	"golang.org/x/term"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Session failed:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("use session serve|start|attach|move|status|cancel|recover")
	}
	action := args[0]
	flags := flag.NewFlagSet("session "+action, flag.ContinueOnError)
	config := flags.String("config", "", "private administrator JSON (serve only)")
	socket := flags.String("socket", "", "private home control socket")
	id := flags.String("id", "", "session UUID (start generates one if omitted)")
	operation := flags.String("operation", "", "stable operation UUID (start/move/cancel generate if omitted)")
	var includeIgnored []string
	flags.Func("include-ignored", "ignored file for every visit; repeatable at start", func(value string) error {
		includeIgnored = append(includeIgnored, value)
		return workspace.ValidateIgnoredPaths(includeIgnored)
	})
	workspacePath := flags.String("workspace", "", "stopped ordinary Git directory (start only)")
	destination := flags.String("to", "", "configured destination name (move only)")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected arguments")
	}
	if action != home.ActionStart && len(includeIgnored) != 0 {
		return errors.New("--include-ignored is immutable start intent")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if action == "serve" {
		if *config == "" || *socket != "" || *id != "" || *operation != "" || *workspacePath != "" || *destination != "" {
			return errors.New("use session serve --config FILE")
		}
		return serve(ctx, *config)
	}
	if *config != "" || *socket == "" {
		return errors.New("client commands require --socket PATH")
	}
	if action == home.ActionStart {
		if *id == "" {
			*id = uuid.NewString()
		}
		if *workspacePath == "" {
			return errors.New("start requires --workspace PATH")
		}
		absolute, err := filepath.Abs(*workspacePath)
		if err != nil {
			return err
		}
		*workspacePath = absolute
	}
	if action == home.ActionStart || action == home.ActionMove || action == home.ActionCancel {
		if *operation == "" {
			*operation = uuid.NewString()
		}
		_, _ = fmt.Fprintf(os.Stderr, "Session %s, operation %s\n", *id, *operation)
	}
	if action == home.ActionAttach {
		if *operation != "" || *workspacePath != "" || *destination != "" {
			return errors.New("attach accepts only --socket and --id")
		}
		return attach(ctx, *socket, *id)
	}
	request := home.Request{
		Action: action, ID: *id, OperationID: *operation, Workspace: *workspacePath, Destination: *destination,
		IncludeIgnored: includeIgnored,
	}
	response, err := home.Call(ctx, *socket, request)
	if response.Status != nil {
		if encodeErr := json.NewEncoder(os.Stdout).Encode(response.Status); encodeErr != nil {
			return encodeErr
		}
	}
	return err
}

func serve(ctx context.Context, path string) error {
	fixed, err := home.ReadConfig(path)
	if err != nil {
		return err
	}
	service, err := home.Open(fixed, os.Stderr)
	if err != nil {
		return err
	}
	defer func() { _ = service.Close() }()
	return service.Serve(ctx)
}

func attach(ctx context.Context, socket, id string) error {
	sizes := make(chan home.TerminalSize, 1)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		state, err := term.MakeRaw(fd)
		if err != nil {
			return err
		}
		defer func() { _ = term.Restore(fd, state) }()
		resize := make(chan os.Signal, 1)
		signal.Notify(resize, syscall.SIGWINCH)
		defer signal.Stop(resize)
		update := func() {
			columns, rows, err := term.GetSize(fd)
			if err == nil && columns > 0 && rows > 0 && columns <= 65535 && rows <= 65535 {
				select {
				case sizes <- home.TerminalSize{Rows: uint16(rows), Columns: uint16(columns)}:
				default:
				}
			}
		}
		update()
		run, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			for {
				select {
				case <-run.Done():
					return
				case <-resize:
					update()
				}
			}
		}()
		_, _ = fmt.Fprintln(os.Stderr, "Attach native Codex; Ctrl-] detaches")
		return home.AttachTerminal(run, socket, id, os.Stdin, os.Stdout, sizes)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Noninteractive input also acknowledges every byte before EOF detaches.
	sizes <- home.TerminalSize{Rows: 24, Columns: 80}
	return home.AttachTerminal(ctx, socket, id, os.Stdin, os.Stdout, sizes)
}
