// Command native-session owns one fixed stock Codex PTY inside a managed agent
// container. Its private socket is a workload-accessible control channel, never
// authority or evidence that every container writer stopped.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
)

const (
	socketPath    = "/tmp/sproozi-native/control.sock"
	configPath    = "/etc/sproozi/native/config.json"
	nativeHome    = "/workspace/session/codex"
	workspace     = "/workspace/session/workspace"
	executable    = "/usr/local/bin/codex"
	opActivate    = "activate"
	opAttach      = "attach"
	opWrite       = "write"
	stateActive   = "active"
	stateStopped  = "stopped"
	stateStopping = "stopping"
)

const (
	opQuiesce         = "quiesce"
	opConfigure       = "configure"
	opStatus          = "status"
	opGate            = "gate"
	statePrepared     = "prepared"
	stateUnconfigured = "unconfigured"
	inactiveError     = "native execution is not active"
)

type config = harness.NativeLaunch
type request struct {
	Op      string      `json:"op"`
	Hook    *claudeHook `json:"hook,omitempty"`
	Config  *config     `json:"config,omitempty"`
	Data    []byte      `json:"data,omitempty"`
	Cursor  uint64      `json:"cursor,omitempty"`
	Rows    uint16      `json:"rows,omitempty"`
	Columns uint16      `json:"columns,omitempty"`
}
type response = harness.NativeReply

type server struct {
	config          config
	deadline        time.Time
	mu              sync.Mutex
	action          sync.Mutex
	native          *nativepty.Supervisor
	state           string
	threadID        string
	done            chan struct{}
	stopOnce        sync.Once
	exitCode        int
	claude          claudeLifecycle
	inputGeneration uint64
	taskBaseline    string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func run(args []string) error {
	if len(args) == 3 && args[0] == "serve" && args[1] == "--config" && args[2] == configPath {
		c, err := loadConfig(configPath)
		if err != nil {
			return err
		}
		return serve(c, false)
	}
	if len(args) == 4 && args[0] == "serve" && args[1] == "--bootstrap" && args[2] == "--deadline" {
		deadline, err := time.Parse(time.RFC3339Nano, args[3])
		if err != nil || !deadline.After(time.Now()) {
			return errors.New("bootstrap requires a future absolute deadline")
		}
		return serve(config{Deadline: deadline}, true)
	}
	if len(args) != 1 {
		return errors.New("use serve --config /etc/sproozi/native/config.json, activate, status, attach, or quiesce")
	}
	switch args[0] {
	case opActivate, opStatus, opAttach, opQuiesce, opConfigure, opGate, opClaudeHook:
		return control(args[0])
	default:
		return errors.New("unknown native session operation")
	}
}
func loadConfig(path string) (config, error) {
	var c config
	f, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return c, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0222 != 0 || info.Size() > 256<<10 {
		return c, errors.New("native launch contract must be a bounded read-only file")
	}
	decoder := json.NewDecoder(io.LimitReader(f, 256<<10))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return c, errors.New("native launch contract has extra JSON")
	}
	return c, c.Validate(time.Now())
}

func serve(c config, bootstrap bool) error {
	if err := os.Mkdir(filepath.Dir(socketPath), 0700); err != nil {
		return fmt.Errorf("create private supervisor directory: %w", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
	if err = os.Chmod(socketPath, 0600); err != nil {
		return err
	}
	s := &server{config: c, deadline: c.Deadline, state: statePrepared, threadID: c.ThreadID, done: make(chan struct{})}
	if bootstrap {
		s.state = stateUnconfigured
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	go func() {
		timer := time.NewTimer(time.Until(c.Deadline))
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
		case <-s.done:
			return
		}
		s.stop()
	}()
	go func() { <-s.done; _ = listener.Close() }()
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				s.mu.Lock()
				code := s.exitCode
				s.mu.Unlock()
				if code != 0 {
					return fmt.Errorf("native container stopped with exit code %d", code)
				}
				return nil
			default:
				return err
			}
		}
		go s.handle(conn)
	}
}
func (s *server) stop() {
	s.mu.Lock()
	n := s.native
	s.state = stateStopped
	s.exitCode = 1
	s.mu.Unlock()
	if n != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		exit, _ := n.Stop(ctx)
		s.mu.Lock()
		s.exitCode = exit.Code
		if s.exitCode == 0 {
			s.exitCode = 1
		}
		s.mu.Unlock()
	}
	s.stopOnce.Do(func() { close(s.done) })
}
func (s *server) configure(c *config) response {
	s.action.Lock()
	defer s.action.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	r := response{Op: opConfigure, State: s.state, ThreadID: s.threadID}
	if c == nil {
		r.Error = "configure requires trusted launch configuration"
		return r
	}
	if !c.Deadline.Equal(s.config.Deadline) {
		r.Error = "configure cannot change initial deadline"
		return r
	}
	if err := c.Validate(time.Now()); err != nil {
		r.Error = err.Error()
		return r
	}
	if s.state != stateUnconfigured {
		old, _ := json.Marshal(s.config)
		newConfig, _ := json.Marshal(c)
		if string(old) != string(newConfig) {
			r.Error = "native configuration is immutable"
		}
		return r
	}
	s.config = *c
	s.threadID = c.ThreadID
	s.state = statePrepared
	r.State, r.ThreadID = s.state, s.threadID
	return r
}
func (s *server) activate() response {
	s.action.Lock()
	defer s.action.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	r := response{Op: opActivate, State: s.state, ThreadID: s.threadID}
	if s.state == stateActive {
		r.PID = s.native.PID()
		return r
	}
	if s.state != statePrepared {
		r.Error = "native launch gate cannot reopen"
		return r
	}
	if !s.config.Deadline.After(time.Now()) {
		r.Error = "native deadline expired"
		return r
	}
	env, err := s.config.Environment()
	if err != nil {
		r.Error = err.Error()
		return r
	}
	clientEnv, err := installNativeClients(s.config, filepath.Dir(harness.NativeLocalTrustBundle))
	if err != nil {
		r.Error = err.Error()
		return r
	}
	env = append(env, clientEnv...)
	home, path, err := nativePaths(s.config.Harness)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	env = append(env, "HOME=/home/agent")
	if s.config.Harness == harness.NativeClaude {
		env = append(env, "CLAUDE_CONFIG_DIR="+home)
	} else {
		env = append(env, "CODEX_HOME="+home)
		if err = installCodexProjectTrust(home); err != nil {
			r.Error = err.Error()
			return r
		}
	}
	if s.config.Task != "" && s.threadID != "" {
		_, baseline, err := observeTurn(nativeHome, s.threadID, s.config.InstructionDigest, s.config.RolloutMaxBytes)
		if err != nil {
			r.Error = "initial task baseline is unavailable: " + err.Error()
			return r
		}
		s.taskBaseline = baseline
	}
	var args []string
	if s.config.Harness == harness.NativeClaude {
		args, env, err = prepareClaudeLaunch(s.config, env)
	} else {
		args = codexArguments(s.config, s.threadID)
	}
	if err != nil {
		r.Error = err.Error()
		return r
	}
	n, err := nativepty.Start(nativepty.Launch{Executable: path, Args: args, Env: env,
		Dir: workspace, Deadline: s.config.Deadline, Size: nativepty.Size{Rows: 40, Columns: 140}})
	if err != nil {
		r.Error = err.Error()
		return r
	}
	s.native, s.state = n, stateActive
	r.PID, r.State = n.PID(), s.state
	go func() {
		<-n.Done()
		exit, waitErr := n.Wait(context.Background())
		s.mu.Lock()
		quiescing := s.state == stateStopping
		if !quiescing {
			s.exitCode = exit.Code
			if waitErr != nil && s.exitCode == 0 {
				s.exitCode = 1
			}
			s.state = stateStopped
		}
		s.mu.Unlock()
		if !quiescing {
			s.stopOnce.Do(func() { close(s.done) })
		}
	}()
	return r
}
func (s *server) status() response {
	s.mu.Lock()
	state := s.state
	id := s.threadID
	n := s.native
	kind := s.config.Harness
	s.mu.Unlock()
	if id == "" && (state == stateActive || state == stateStopping) {
		observed, _, err := s.observeNativeTurn()
		if err == nil {
			s.mu.Lock()
			if s.threadID == "" {
				s.threadID = observed
			}
			id = s.threadID
			s.mu.Unlock()
		}
	}
	r := response{Op: opStatus, State: state, ThreadID: id}
	if kind == harness.NativeClaude {
		r.Completion = s.claudeCompletion()
	}
	if n != nil {
		r.PID = n.PID()
	}
	return r
}
func (s *server) gate() response {
	s.action.Lock()
	defer s.action.Unlock()
	s.mu.Lock()
	n := s.native
	if s.state != stateActive &&
		s.state != stateStopping {
		r := response{Op: opGate, State: s.state, Error: inactiveError}
		s.mu.Unlock()
		return r
	}
	s.state = stateStopping
	thread := s.threadID
	s.mu.Unlock()
	n.GateInput()
	return response{Op: opGate, State: stateStopping, ThreadID: thread}
}
func (s *server) quiesce() response {
	s.action.Lock()
	defer s.action.Unlock()
	s.mu.Lock()
	n := s.native
	if s.state != stateActive &&
		s.state != stateStopping {
		r := response{Op: opQuiesce, State: s.state, Error: inactiveError}
		s.mu.Unlock()
		return r
	}
	s.state = stateStopping
	s.mu.Unlock()
	n.GateInput()
	ctx, cancel := context.WithDeadline(context.Background(), s.deadline)
	defer cancel()
	timer := time.NewTicker(100 * time.Millisecond)
	defer timer.Stop()
	var id, turn string
	for {
		var err error
		id, turn, err = s.observeNativeTurn()
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			s.stop()
			return response{Op: opQuiesce, State: stateStopping, Error: "no completed safe native turn before deadline"}
		case <-n.Done():
			s.stop()
			return response{Op: opQuiesce, State: stateStopping,
				Error: "native process stopped before completed turn observation"}
		case <-timer.C:
		}
	}
	var exit nativepty.Exit
	var err error
	if s.config.Harness == harness.NativeClaude {
		exit, err = n.Stop(ctx)
	} else {
		exit, err = n.QuitAfterCompletedTurn(ctx)
	}
	if err != nil {
		s.stop()
		return response{Op: opQuiesce, State: stateStopping, Error: err.Error()}
	}
	s.mu.Lock()
	s.threadID, s.state = id, stateStopped
	s.exitCode = exit.Code
	if s.config.Harness == harness.NativeClaude {
		s.exitCode = 0
	}
	if s.config.Harness != harness.NativeClaude && exit.Reason != nativepty.NativeExit && s.exitCode == 0 {
		s.exitCode = 1
	}
	s.mu.Unlock()
	r := response{Op: opQuiesce, State: stateStopped, ThreadID: id, TurnID: turn,
		Exit: &harness.NativeExit{Code: exit.Code, Reason: string(exit.Reason)}}
	if s.config.Harness == harness.NativeClaude {
		if exit.Reason != nativepty.ForcedStop {
			r.Error = "native controlled stop did not finish"
		}
	} else if exit.Code != 0 || exit.Reason != nativepty.NativeExit {
		r.Error = "native quit did not exit normally"
	}
	return r
}
func (s *server) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), 256<<10)
	if !scanner.Scan() {
		return
	}
	var q request
	if err := strictRequest(scanner.Bytes(), &q); err != nil {
		_ = json.NewEncoder(conn).Encode(response{Error: err.Error()})
		return
	}
	encoder := json.NewEncoder(conn)
	switch q.Op {
	case opClaudeHook:
		_ = encoder.Encode(s.claudeHook(q.Hook))
	case opConfigure:
		_ = encoder.Encode(s.configure(q.Config))
	case opGate:
		_ = encoder.Encode(s.gate())
	case opActivate:
		_ = encoder.Encode(s.activate())
	case opStatus:
		_ = encoder.Encode(s.status())
	case opQuiesce:
		_ = conn.SetDeadline(s.deadline)
		r := s.quiesce()
		_ = encoder.Encode(r)
		if r.State == stateStopped {
			s.stopOnce.Do(func() { close(s.done) })
		}
	case opAttach:
		_ = conn.SetDeadline(s.deadline)
		s.attach(conn, scanner, encoder, q.Cursor)
	default:
		_ = encoder.Encode(response{Error: "unknown fixed native operation"})
	}
}
func strictRequest(data []byte, q *request) error {
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err := d.Decode(q); err != nil {
		return err
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return errors.New("extra control JSON")
	}
	return nil
}
func (s *server) attach(conn net.Conn, scanner *bufio.Scanner, encoder *json.Encoder, cursor uint64) {
	s.mu.Lock()
	n, state := s.native, s.state
	s.mu.Unlock()
	if n == nil || state != stateActive {
		_ = encoder.Encode(response{Op: opAttach, Error: inactiveError})
		return
	}
	a, err := n.Attach(cursor)
	if err != nil {
		_ = encoder.Encode(response{Op: opAttach, Error: err.Error()})
		return
	}
	defer func() { _ = a.Close() }()
	var write sync.Mutex
	emit := func(r response) error {
		write.Lock()
		defer write.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
		return encoder.Encode(r)
	}
	if emit(response{Op: opAttach, PID: n.PID(), State: state}) != nil {
		return
	}
	ctx, cancel := context.WithDeadline(context.Background(), s.deadline)
	defer cancel()
	go func() {
		defer cancel()
		defer func() { _ = conn.Close() }()
		for {
			out, err := a.Read(ctx)
			if err != nil {
				return
			}
			if emit(response{Op: "output", Data: out.Data, Cursor: out.Next, Gap: out.Gap}) != nil || out.Exited {
				return
			}
		}
	}()
	for scanner.Scan() {
		var q request
		err := strictRequest(scanner.Bytes(), &q)
		r := response{Op: q.Op}
		if err == nil {
			switch q.Op {
			case opWrite:
				r.Written, err = s.writeInput(a, q.Data)
			case "resize":
				err = a.Resize(nativepty.Size{Rows: q.Rows, Columns: q.Columns})
			default:
				err = errors.New("attachment permits only write and resize")
			}
		}
		if err != nil {
			r.Error = err.Error()
		}
		if emit(r) != nil {
			return
		}
	}
}
func control(op string) error {
	conn, err := net.DialTimeout("unix", socketPath, 3*time.Second)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	q := request{Op: op}
	if op == opClaudeHook {
		q.Hook, err = readClaudeHook(os.Stdin)
		if err != nil {
			return err
		}
	}
	if op == opConfigure {
		var c config
		decoder := json.NewDecoder(io.LimitReader(os.Stdin, 256<<10))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&c); err != nil {
			return err
		}
		var extra any
		if decoder.Decode(&extra) != io.EOF {
			return errors.New("extra native configure JSON")
		}
		q.Config = &c
	}
	if err = json.NewEncoder(conn).Encode(q); err != nil {
		return err
	}
	if op == opAttach {
		go func() { _, _ = io.Copy(conn, os.Stdin); _ = conn.Close() }()
		_, err = io.Copy(os.Stdout, conn)
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
	var r response
	if err = json.NewDecoder(io.LimitReader(conn, 64<<10)).Decode(&r); err != nil {
		return err
	}
	if op == opClaudeHook {
		if r.Error != "" {
			return errors.New(r.Error)
		}
		// Claude command hooks expect no unrelated JSON on success.
		return nil
	}
	if err = json.NewEncoder(os.Stdout).Encode(r); err != nil {
		return err
	}
	if r.Error != "" {
		return errors.New(r.Error)
	}
	return nil
}
