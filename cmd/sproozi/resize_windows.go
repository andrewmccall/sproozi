package main

import (
	"context"
	"time"
)

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
