package main

import (
	"context"
	"os"
	"time"

	"golang.org/x/term"
)

// Windows console dimensions belong to a screen-buffer handle, not stdin's
// input-buffer handle. stderr remains usable when stdout is redirected.
func terminalSize(_ int) (int, int, error) {
	columns, rows, err := term.GetSize(int(os.Stdout.Fd()))
	if err == nil {
		return columns, rows, nil
	}
	return term.GetSize(int(os.Stderr.Fd()))
}

// Console resize events share the input queue with terminal keystrokes. Polling
// dimensions leaves stdin with AttachTerminal, which owns that input stream.
func watchResize(ctx context.Context, update func()) func() {
	ctx, cancel := context.WithCancel(ctx)
	update()
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				update()
			}
		}
	}()
	return cancel
}
