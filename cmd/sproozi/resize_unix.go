//go:build !windows

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

func terminalSize(fd int) (int, int, error) { return term.GetSize(fd) }

func watchResize(ctx context.Context, update func()) func() {
	ctx, cancel := context.WithCancel(ctx)
	resize := make(chan os.Signal, 1)
	signal.Notify(resize, syscall.SIGWINCH)
	update()
	go func() {
		defer signal.Stop(resize)
		for {
			select {
			case <-ctx.Done():
				return
			case <-resize:
				update()
			}
		}
	}()
	return cancel
}
