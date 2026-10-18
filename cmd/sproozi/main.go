// Command sproozi owns the trusted home daemon and its local terminal client.
package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/andrewmccall/sproozi/internal/session/home"
	"golang.org/x/term"
)

func main() {
	os.Exit(cli(os.Args[1:], os.Stdout, os.Stderr))
}

// cli runs one command line and returns its exit status. Help exits 0.
func cli(args []string, stdout, stderr io.Writer) int {
	if err := run(args, stdout); err != nil {
		_, _ = fmt.Fprintln(stderr, "Sproozi failed:", err)
		return 1
	}
	return 0
}

func run(args []string, stdout io.Writer) error {
	if len(args) == 1 && args[0] == "--version" {
		args = []string{actionVersion}
	}
	if len(args) == 0 || helpRequested(args[0]) {
		usage(stdout)
		return nil
	}
	return runProduct(args, stdout)
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
		_, _ = fmt.Fprintln(os.Stderr, "Attach native terminal; Ctrl-] detaches")
		return home.AttachTerminal(run, socket, id, os.Stdin, os.Stdout, sizes)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Noninteractive input also acknowledges every byte before EOF detaches.
	sizes <- home.TerminalSize{Rows: 24, Columns: 80}
	return home.AttachTerminal(ctx, socket, id, os.Stdin, os.Stdout, sizes)
}
