//go:build linux || darwin

// Package nativepty keeps a native terminal process alive across authenticated
// client detachments. Its caller owns launch authority, authentication and native
// completed-turn detection. It exposes no socket or history parser to workloads.
// Process-group termination does not fence tools that escape with setsid or
// double-fork. Before capture, the runtime must stop and inspect the actual Docker
// container or observe Kubernetes workload-container termination independently.
package nativepty

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

var (
	ErrAttached  = errors.New("native terminal already has an input attachment")
	ErrDetached  = errors.New("native terminal attachment was detached")
	ErrGated     = errors.New("native terminal input is gated")
	ErrExited    = errors.New("native terminal process exited")
	ErrInputBusy = errors.New("native terminal input buffer is full")
	ErrCursor    = errors.New("native terminal output cursor exceeds produced output")
)

// Launch contains only trusted executable configuration. Env must be supplied
// explicitly; the supervisor never inherits the launching user's credentials.
type Launch struct {
	Executable  string
	Args        []string
	Env         []string
	Dir         string
	Deadline    time.Time
	Size        Size
	ReplayBytes int
}

type Size struct{ Rows, Columns uint16 }

type StopReason string

const (
	NativeExit StopReason = "native-exit"
	Deadline   StopReason = "deadline"
	ForcedStop StopReason = "forced-stop"
)

// Exit is available after leader exit, owned-group stop observation and output
// drain. A Wait error means group stopping could not be confirmed. Even a nil
// error is not evidence of a container-wide writer fence or safe native state.
type Exit struct {
	Code   int
	Reason StopReason
}

// Output is a copied replay segment. Gap reports that a slow/disconnected reader
// missed bytes; Next remains a monotonic cursor in this supervisor incarnation.
type Output struct {
	Data        []byte
	Next        uint64
	Gap, Exited bool
}

type Supervisor struct {
	mu         sync.Mutex
	input      sync.Mutex
	master     *os.File
	cmd        *exec.Cmd
	buffer     []byte
	limit      int
	offset     uint64
	notify     chan struct{}
	done       chan struct{}
	readerDone chan struct{}
	active     *Attachment
	gated      bool
	quitting   bool
	exited     bool
	exit       Exit
	exitErr    error
}

// Attachment is the sole input lease. Closing it leaves the native process and
// replay alive. Output readers cannot block the PTY's output-draining goroutine.
type Attachment struct {
	supervisor *Supervisor
	closed     chan struct{}
	read       sync.Mutex
	cursor     uint64
}

func Start(launch Launch) (*Supervisor, error) {
	if !filepath.IsAbs(launch.Executable) || !filepath.IsAbs(launch.Dir) || launch.Env == nil || !launch.Deadline.After(time.Now()) {
		return nil, errors.New("native terminal requires an absolute executable/workspace, explicit environment and future deadline")
	}
	if launch.Size.Rows == 0 || launch.Size.Columns == 0 {
		return nil, errors.New("native terminal dimensions must be positive")
	}
	if launch.ReplayBytes == 0 {
		launch.ReplayBytes = 64 << 10
	}
	if launch.ReplayBytes < 1 || launch.ReplayBytes > 1<<20 {
		return nil, errors.New("native terminal replay must fit between one byte and one MiB")
	}
	master, slave, err := openPTY()
	if err != nil {
		return nil, fmt.Errorf("open native terminal: %w", err)
	}
	if err = resize(master, launch.Size); err != nil {
		_ = master.Close()
		_ = slave.Close()
		return nil, fmt.Errorf("size native terminal: %w", err)
	}
	cmd := exec.Command(launch.Executable, launch.Args...)
	cmd.Env, cmd.Dir = append([]string{}, launch.Env...), launch.Dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err = cmd.Start(); err != nil {
		_ = master.Close()
		_ = slave.Close()
		return nil, fmt.Errorf("start native terminal: %w", err)
	}
	_ = slave.Close()
	s := &Supervisor{master: master, cmd: cmd, limit: launch.ReplayBytes,
		notify: make(chan struct{}), done: make(chan struct{}), readerDone: make(chan struct{}),
		exit: Exit{Reason: NativeExit}}
	go s.drain()
	go s.reap()
	go func() {
		timer := time.NewTimer(time.Until(launch.Deadline))
		defer timer.Stop()
		select {
		case <-s.done:
		case <-timer.C:
			s.force(Deadline)
		}
	}()
	return s, nil
}

func (s *Supervisor) PID() int              { return s.cmd.Process.Pid }
func (s *Supervisor) Done() <-chan struct{} { return s.done }

func (s *Supervisor) Attach(cursor uint64) (*Attachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil {
		return nil, ErrAttached
	}
	if cursor > s.offset {
		return nil, ErrCursor
	}
	a := &Attachment{supervisor: s, closed: make(chan struct{}), cursor: cursor}
	s.active = a
	return a, nil
}

func (a *Attachment) Close() error {
	s := a.supervisor
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == a {
		s.active = nil
		close(a.closed)
	}
	return nil
}

func (a *Attachment) Read(ctx context.Context) (Output, error) {
	a.read.Lock()
	defer a.read.Unlock()
	s := a.supervisor
	for {
		s.mu.Lock()
		if s.active != a {
			s.mu.Unlock()
			return Output{}, ErrDetached
		}
		start := s.offset - uint64(len(s.buffer))
		gap := a.cursor < start
		if gap {
			a.cursor = start
		}
		if a.cursor < s.offset || s.exited {
			data := append([]byte(nil), s.buffer[a.cursor-start:]...)
			a.cursor = s.offset
			out := Output{Data: data, Next: s.offset, Gap: gap, Exited: s.exited}
			s.mu.Unlock()
			return out, nil
		}
		notify := s.notify
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return Output{}, ctx.Err()
		case <-a.closed:
			return Output{}, ErrDetached
		case <-notify:
		}
	}
}

// Write forwards bounded terminal bytes without waiting for a slow native
// reader. The caller must retain any unwritten suffix when ErrInputBusy occurs.
func (a *Attachment) Write(data []byte) (int, error) {
	if len(data) > 4096 {
		return 0, errors.New("native terminal input segment exceeds 4096 bytes")
	}
	s := a.supervisor
	s.input.Lock()
	defer s.input.Unlock()
	s.mu.Lock()
	if s.active != a {
		s.mu.Unlock()
		return 0, ErrDetached
	}
	if s.exited {
		s.mu.Unlock()
		return 0, ErrExited
	}
	if s.gated {
		s.mu.Unlock()
		return 0, ErrGated
	}
	s.mu.Unlock()
	return writeInput(s.master, data)
}

func (a *Attachment) Resize(size Size) error {
	if size.Rows == 0 || size.Columns == 0 {
		return errors.New("native terminal dimensions must be positive")
	}
	s := a.supervisor
	s.input.Lock()
	defer s.input.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != a {
		return ErrDetached
	}
	if s.exited {
		return ErrExited
	}
	return resize(s.master, size)
}

// GateInput returns after any previously admitted write. Subsequent attachment
// input is rejected immediately, while the active native turn keeps running.
func (s *Supervisor) GateInput() {
	s.input.Lock()
	defer s.input.Unlock()
	s.mu.Lock()
	s.gated = true
	s.mu.Unlock()
}

// QuitAfterCompletedTurn requires the caller to gate input, observe a genuine
// completed native turn and resolve pending tools first. It does not infer that
// boundary from terminal text. It discards an unsubmitted composer draft, sends
// native /quit with a separate Enter event, then confirms process-group stop.
// Cancellation forces termination; such a stop is not a safe checkpoint proof.
func (s *Supervisor) QuitAfterCompletedTurn(ctx context.Context) (Exit, error) {
	s.input.Lock()
	s.mu.Lock()
	gated, exited := s.gated, s.exited
	if gated && !exited {
		s.quitting = true
	}
	s.mu.Unlock()
	if !gated {
		s.input.Unlock()
		return Exit{}, errors.New("native quit requires gated input and a caller-verified completed turn")
	}
	if exited {
		s.input.Unlock()
		return s.Wait(ctx)
	}
	n, err := writeInput(s.master, []byte("\x15/quit"))
	if err != nil || n != 6 {
		s.input.Unlock()
		return Exit{}, fmt.Errorf("send native quit: %w", errors.Join(err, io.ErrShortWrite))
	}
	s.input.Unlock()
	// Pinned native TUI paste handling requires Enter to be a distinct key event.
	timer := time.NewTimer(300 * time.Millisecond)
	select {
	case <-ctx.Done():
		timer.Stop()
		s.force(ForcedStop)
		return Exit{}, ctx.Err()
	case <-s.done:
		timer.Stop()
		return s.Wait(ctx)
	case <-timer.C:
	}
	s.input.Lock()
	s.mu.Lock()
	exited = s.exited
	s.mu.Unlock()
	if exited {
		s.input.Unlock()
		return s.Wait(ctx)
	}
	n, err = writeInput(s.master, []byte("\r"))
	s.input.Unlock()
	if err != nil || n != 1 {
		return Exit{}, fmt.Errorf("submit native quit: %w", errors.Join(err, io.ErrShortWrite))
	}
	exit, err := s.Wait(ctx)
	if ctx.Err() != nil {
		s.force(ForcedStop)
	}
	return exit, err
}

func (s *Supervisor) Stop(ctx context.Context) (Exit, error) { s.force(ForcedStop); return s.Wait(ctx) }

func (s *Supervisor) Wait(ctx context.Context) (Exit, error) {
	select {
	case <-ctx.Done():
		return Exit{}, ctx.Err()
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.exit, s.exitErr
	}
}

func (s *Supervisor) force(reason StopReason) {
	s.input.Lock()
	defer s.input.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited {
		return
	}
	s.gated = true
	s.exit.Reason = reason
	_ = unix.Kill(-s.PID(), unix.SIGKILL)
}

func (s *Supervisor) drain() {
	defer close(s.readerDone)
	buf := make([]byte, 4096)
	for {
		n, err := s.master.Read(buf)
		if n > 0 {
			s.mu.Lock()
			s.buffer = append(s.buffer, buf[:n]...)
			if len(s.buffer) > s.limit {
				s.buffer = append([]byte(nil), s.buffer[len(s.buffer)-s.limit:]...)
			}
			s.offset += uint64(n)
			close(s.notify)
			s.notify = make(chan struct{})
			s.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (s *Supervisor) reap() {
	_ = s.cmd.Wait()
	// A native leader can exit with descendants still in its group. Terminate
	// those before publishing stopped; an engine/cgroup adapter must separately
	// confirm descendants that deliberately escaped this process group.
	err := stopGroup(s.PID())
	select {
	case <-s.readerDone:
	case <-time.After(100 * time.Millisecond):
	}
	s.input.Lock()
	_ = s.master.Close()
	s.input.Unlock()
	<-s.readerDone
	s.mu.Lock()
	s.exit.Code = s.cmd.ProcessState.ExitCode()
	s.exitErr, s.exited, s.gated = err, true, true
	close(s.notify)
	close(s.done)
	s.mu.Unlock()
}

func writeInput(file *os.File, data []byte) (int, error) {
	connection, err := file.SyscallConn()
	if err != nil {
		return 0, err
	}
	var n int
	var writeErr error
	err = connection.Control(func(fd uintptr) { n, writeErr = unix.Write(int(fd), data) })
	if err != nil {
		return 0, err
	}
	err = writeErr
	if n < 0 {
		n = 0
	}
	if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK) {
		return n, ErrInputBusy
	}
	return n, err
}

func resize(file *os.File, size Size) error {
	connection, err := file.SyscallConn()
	if err != nil {
		return err
	}
	var sizeErr error
	err = connection.Control(func(fd uintptr) {
		sizeErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Row: size.Rows, Col: size.Columns})
	})
	if err != nil {
		return err
	}
	return sizeErr
}

func stopGroup(pid int) error {
	if err := unix.Kill(-pid, unix.SIGKILL); err != nil && !errors.Is(err, unix.ESRCH) {
		return err
	}
	end := time.Now().Add(2 * time.Second)
	for {
		stopped, err := groupStopped(pid)
		if stopped {
			return nil
		}
		if err != nil {
			return err
		}
		if time.Now().After(end) {
			return errors.New("native process group stop could not be confirmed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
