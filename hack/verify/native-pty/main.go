// Command native-pty drives the production supervisor for the stock TUI fixture.
// It is a verification helper, not a service or an authority endpoint.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
)

type request struct {
	Op          string            `json:"op"`
	Home        string            `json:"home,omitempty"`
	ThreadID    string            `json:"threadId,omitempty"`
	RestoreHome string            `json:"restoreHome,omitempty"`
	Launch      *nativepty.Launch `json:"launch,omitempty"`
	Data        []byte            `json:"data,omitempty"`
	Cursor      uint64            `json:"cursor,omitempty"`
	Size        nativepty.Size    `json:"size,omitempty"`
}
type response struct {
	Digest  string          `json:"digest,omitempty"`
	Error   string          `json:"error,omitempty"`
	PID     int             `json:"pid,omitempty"`
	Data    []byte          `json:"data,omitempty"`
	Cursor  uint64          `json:"cursor,omitempty"`
	Gap     bool            `json:"gap,omitempty"`
	Exited  bool            `json:"exited,omitempty"`
	Written int             `json:"written,omitempty"`
	Exit    *nativepty.Exit `json:"exit,omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run() error {
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	encoder := json.NewEncoder(os.Stdout)
	var s *nativepty.Supervisor
	var a *nativepty.Attachment
	defer func() {
		if s != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_, _ = s.Stop(ctx)
		}
	}()
	for scanner.Scan() {
		var q request
		var r response
		err := json.Unmarshal(scanner.Bytes(), &q)
		if err == nil {
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			switch q.Op {
			case "start":
				if s != nil || q.Launch == nil {
					err = errors.New("start requires one launch")
				} else {
					s, err = nativepty.Start(*q.Launch)
					if err == nil {
						r.PID = s.PID()
					}
				}
			case "attach":
				if s == nil {
					err = errors.New("not started")
				} else if a != nil {
					err = nativepty.ErrAttached
				} else {
					a, err = s.Attach(q.Cursor)
				}
			case "detach":
				if a == nil {
					err = nativepty.ErrDetached
				} else {
					err = a.Close()
					a = nil
				}
			case "read":
				if a == nil {
					err = nativepty.ErrDetached
				} else {
					var out nativepty.Output
					out, err = a.Read(ctx)
					if errors.Is(err, context.DeadlineExceeded) {
						err = nil
					}
					r.Data, r.Cursor, r.Gap, r.Exited = out.Data, out.Next, out.Gap, out.Exited
				}
			case "write":
				if a == nil {
					err = nativepty.ErrDetached
				} else {
					r.Written, err = a.Write(q.Data)
				}
			case "resize":
				if a == nil {
					err = nativepty.ErrDetached
				} else {
					err = a.Resize(q.Size)
				}
			case "gate":
				if s == nil {
					err = errors.New("not started")
				} else {
					s.GateInput()
				}
			case "status":
				if s == nil {
					err = errors.New("not started")
				} else {
					r.PID = s.PID()
					select {
					case <-s.Done():
						r.Exited = true
					default:
					}
				}
			case "checkpoint":
				r, err = transferCheckpoint(s, q)
			case "quit", "stop", "wait":
				r, err = finishNative(s, q.Op)
			default:
				err = errors.New("unknown fixture operation")
			}
			cancel()
		}
		if err != nil {
			r.Error = err.Error()
		}
		if err = encoder.Encode(r); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func transferCheckpoint(s *nativepty.Supervisor, q request) (response, error) {
	var r response
	if s == nil {
		return r, errors.New("not started")
	}
	select {
	case <-s.Done():
	default:
		return r, errors.New("checkpoint requires stopped native process")
	}
	state, err := harness.CaptureCodexState(q.Home, q.ThreadID, 8<<20)
	if err != nil {
		return r, err
	}
	if _, err = harness.CompletedCodexTurn(state, 8<<20); err != nil {
		return r, err
	}
	if err = harness.RestoreCodexState(q.RestoreHome, state, 8<<20); err != nil {
		return r, err
	}
	r.Digest = fmt.Sprintf("%x", sha256.Sum256(state.Rollout))
	return r, nil
}
func finishNative(s *nativepty.Supervisor, op string) (response, error) {
	var r response
	if s == nil {
		return r, errors.New("not started")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var exit nativepty.Exit
	var err error
	switch op {
	case "quit":
		exit, err = s.QuitAfterCompletedTurn(ctx)
	case "stop":
		exit, err = s.Stop(ctx)
	case "wait":
		exit, err = s.Wait(ctx)
	}
	if err == nil {
		r.Exit = &exit
		r.Exited = true
	}
	return r, err
}
