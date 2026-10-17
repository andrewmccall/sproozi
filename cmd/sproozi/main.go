// Command sproozi owns the trusted home daemon and its local terminal client.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/andrewmccall/sproozi/internal/session/home"
	"golang.org/x/term"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Sproozi failed:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return errors.New("use sproozi run|sessions|pull|resume|stop|serve|start|attach|move|status|cancel|recover")
	}
	if args[0] != "serve" {
		return runProduct(args)
	}
	flags := flag.NewFlagSet("sproozi serve", flag.ContinueOnError)
	config := flags.String("config", "", "private administrator JSON")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if *config == "" || flags.NArg() != 0 {
		return errors.New("use sproozi serve --config FILE")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return serve(ctx, *config)
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
		_, _ = fmt.Fprintln(os.Stderr, "Attach native terminal; Ctrl-] detaches")
		return home.AttachTerminal(run, socket, id, os.Stdin, os.Stdout, sizes)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Noninteractive input also acknowledges every byte before EOF detaches.
	sizes <- home.TerminalSize{Rows: 24, Columns: 80}
	return home.AttachTerminal(ctx, socket, id, os.Stdin, os.Stdout, sizes)
}
