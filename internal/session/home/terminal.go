package home

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// TerminalSize comes from the user's terminal, never checkpoint metadata.
type TerminalSize struct{ Rows, Columns uint16 }

const (
	terminalWrite  = "write"
	terminalResize = "resize"
)

type terminalFrame struct {
	Op      string `json:"op"`
	Data    []byte `json:"data,omitempty"`
	Rows    uint16 `json:"rows,omitempty"`
	Columns uint16 `json:"columns,omitempty"`
	Written int    `json:"written,omitempty"`
	Error   string `json:"error,omitempty"`
	Gap     bool   `json:"gap,omitempty"`
}

// AttachTerminal converts byte input to the fixed native terminal frames. Ctrl-]
// detaches locally. Partial/busy writes retain their suffix; closing the socket
// ends only this attachment, while the home daemon and native process continue.
func AttachTerminal(ctx context.Context, path, id string, input io.Reader, output io.Writer, sizes <-chan TerminalSize) error {
	conn, reader, err := connect(ctx, path, Request{Action: ActionAttach, ID: id})
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(run, func() { _ = conn.Close() })
	defer stop()
	line, err := readLine(reader, controlLimit)
	if err != nil {
		return err
	}
	var reply Response
	if err = decode(line, &reply); err != nil {
		return err
	}
	if err = replyError(reply); err != nil {
		return err
	}
	var writer sync.Mutex
	encoder := json.NewEncoder(conn)
	emit := func(f terminalFrame) error { writer.Lock(); defer writer.Unlock(); return encoder.Encode(f) }
	acks := make(chan terminalFrame, 1)
	inputDone := make(chan error, 1)
	go func() { inputDone <- terminalInput(run, input, emit, acks); cancel() }()
	go func() {
		for {
			select {
			case <-run.Done():
				return
			case size, ok := <-sizes:
				if !ok {
					return
				}
				if size.Rows > 0 && size.Columns > 0 {
					if emit(terminalFrame{Op: terminalResize, Rows: size.Rows, Columns: size.Columns}) != nil {
						return
					}
				}
			}
		}
	}()
	for {
		line, err = readLine(reader, 256<<10)
		if err != nil {
			select {
			case result := <-inputDone:
				return result
			default:
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var f terminalFrame
		// Native output includes cursor, state and exit metadata that this byte
		// terminal does not interpret. Validate the JSON, then consume its fields.
		if err = json.Unmarshal(line, &f); err != nil {
			return err
		}
		if f.Op == terminalWrite {
			select {
			case acks <- f:
			case <-run.Done():
				return nil
			}
			continue
		}
		if f.Error != "" {
			return terminalFailure(id, f)
		}
		if len(f.Data) > 0 {
			if _, err = output.Write(f.Data); err != nil {
				return err
			}
		}
		if f.Gap {
			if _, err = io.WriteString(output, "\r\n[Earlier terminal output is no longer retained]\r\n"); err != nil {
				return err
			}
		}
	}
}

// inactiveNative is the supervisor's reply when no native process runs.
const inactiveNative = "native execution is not active"

// terminalFailure names a failed native terminal frame and the next step. The
// reason comes from inside the sandbox, so only printable ASCII is shown.
func terminalFailure(id string, f terminalFrame) error {
	if f.Error == inactiveNative {
		return fmt.Errorf("home: session %s has no running native process to attach to; run \"sproozi status %s\" "+
			"to see its phase: \"sproozi recover %s\" finishes an unfinished start or move, and "+
			"\"sproozi resume %s\" continues a stopped session", id, id, id, id)
	}
	op := "operation"
	if f.Op == "attach" || f.Op == terminalResize {
		op = f.Op
	}
	reason := strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return -1
		}
		return r
	}, f.Error)
	if len(reason) > 200 {
		reason = reason[:200] + "…"
	}
	if f.Op == "" {
		// The home daemon's own frame already names itself.
		return fmt.Errorf("%s; run \"sproozi status %s\" to see the session's phase", reason, id)
	}
	return fmt.Errorf("home: native terminal %s failed: %s; run \"sproozi status %s\" to see the session's phase",
		op, reason, id)
}

func terminalInput(ctx context.Context, input io.Reader, emit func(terminalFrame) error, acks <-chan terminalFrame) error {
	data := make([]byte, 4096)
	for {
		n, err := input.Read(data)
		detached := false
		if at := bytes.IndexByte(data[:n], 0x1d); at >= 0 {
			n = at
			detached = true
		}
		pending := data[:n]
		for len(pending) > 0 {
			if writeErr := emit(terminalFrame{Op: terminalWrite, Data: pending}); writeErr != nil {
				return writeErr
			}
			var reply terminalFrame
			select {
			case reply = <-acks:
			case <-ctx.Done():
				return ctx.Err()
			}
			if reply.Written < 0 || reply.Written > len(pending) {
				return errors.New("home: invalid terminal acknowledgement")
			}
			pending = pending[reply.Written:]
			if reply.Error != "" && reply.Error != "native terminal input buffer is full" {
				return errors.New("home: native input failed")
			}
			if reply.Written == 0 {
				timer := time.NewTimer(10 * time.Millisecond)
				select {
				case <-timer.C:
				case <-ctx.Done():
					timer.Stop()
					return ctx.Err()
				}
			}
		}
		if detached || errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}
