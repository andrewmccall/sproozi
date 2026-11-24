//go:build linux || darwin

package nativepty_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/nativepty"
	"golang.org/x/sys/unix"
)

// BeforeKill runs before each kill is sent, while the process still runs:
// for a stop, for a quit whose caller gave up, and at the deadline.
func TestBeforeKillRunsBeforeEveryKill(t *testing.T) {
	for name, kill := range map[string]func(*nativepty.Supervisor){
		"stop": func(s *nativepty.Supervisor) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, _ = s.Stop(ctx)
		},
		"quit given up": func(s *nativepty.Supervisor) {
			s.GateInput()
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			_, _ = s.QuitAfterCompletedTurn(ctx)
		},
		"deadline": func(s *nativepty.Supervisor) { <-s.Done() },
	} {
		t.Run(name, func(t *testing.T) {
			until := time.Time{}
			if name == "deadline" {
				until = time.Now().Add(300 * time.Millisecond)
			}
			var calls, whileRunning atomic.Int32
			var s *nativepty.Supervisor
			var pid atomic.Int32
			s, err := nativepty.Start(nativepty.Launch{Executable: "/bin/sh", Args: []string{"-c", "exec sleep 30"},
				Env: []string{"PATH=/usr/bin:/bin", "TERM=xterm"}, Dir: t.TempDir(), Deadline: until,
				Size: nativepty.Size{Rows: 24, Columns: 80}, BeforeKill: func() {
					calls.Add(1)
					if unix.Kill(int(pid.Load()), 0) == nil {
						whileRunning.Add(1)
					}
				}})
			if err != nil {
				t.Fatal(err)
			}
			pid.Store(int32(s.PID()))
			kill(s)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if _, err = s.Wait(ctx); err != nil {
				t.Fatal(err)
			}
			if calls.Load() == 0 || whileRunning.Load() != calls.Load() {
				t.Fatalf("BeforeKill ran %d times, %d while the process ran", calls.Load(), whileRunning.Load())
			}
		})
	}
}
