// Command native-session owns one fixed stock Codex or Claude Code PTY inside a
// managed agent container. Its private socket is a workload-accessible control
// channel, never authority or evidence that every container writer stopped.
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
	"github.com/google/uuid"
)

const (
	socketPath    = "/tmp/sproozi-native/control.sock"
	configPath    = "/etc/sproozi/native/config.json"
	nativeHome    = "/workspace/session/codex"
	workspace     = "/workspace/session/workspace"
	executable    = "/usr/local/bin/codex"
	agentHome     = "/home/agent"
	inactiveError = "native execution is not active"
)

// Attachment frames and the Claude hook use the supervisor socket as well. The
// placement-facing operations and states are defined once in harness.
const (
	opWrite  harness.NativeOp = "write"
	opResize harness.NativeOp = "resize"
	opOutput harness.NativeOp = "output"
)

type config = harness.NativeLaunch
type request struct {
	Op      harness.NativeOp `json:"op"`
	Hook    *claudeHook      `json:"hook,omitempty"`
	Config  *config          `json:"config,omitempty"`
	Data    []byte           `json:"data,omitempty"`
	Cursor  uint64           `json:"cursor,omitempty"`
	Rows    uint16           `json:"rows,omitempty"`
	Columns uint16           `json:"columns,omitempty"`
}
type response = harness.NativeReply

type server struct {
	config   config
	deadline time.Time
	mu       sync.Mutex
	action   sync.Mutex
	native   *nativepty.Supervisor
	state    harness.NativeState
	threadID string
	// launchID is the conversation a new visit's supervisor named itself,
	// for a harness that takes its ID at launch (Copilot).
	launchID string
	// login keeps the harness's sign-in in step with its login store.
	login    *loginSync
	done     chan struct{}
	stopOnce sync.Once
	// cause is why the harness ended, decided once (decide). quiescing marks
	// a graceful stop that owns the harness's exit; ended records that the
	// exit is settled, with the harness's code and the supervisor's own.
	cause       exitCause
	quiescing   bool
	ended       bool
	harnessCode int
	exitCode    int
	// reports receives exit reports; nil is standard output.
	reports         io.Writer
	claude          claudeLifecycle
	inputGeneration uint64
	taskBaseline    string
	// gatedAt is when input was first gated; turnless latches the decision
	// that this first visit has no native conversation to keep.
	gatedAt  time.Time
	turnless bool
	// lastInputAt is when the last terminal write was accepted.
	lastInputAt time.Time
}

// quiesceWait bounds one quiesce request's wait for a completed turn.
var quiesceWait = harness.NativeQuiesceWait

// turnlessSettle is how long gated input must stay quiet before a first visit
// with no native conversation counts as turnless. It covers a prompt
// submitted just before the gate whose native record has not appeared yet.
var turnlessSettle = 2 * time.Second

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
	switch op := harness.NativeOp(args[0]); op {
	case harness.NativeActivate, harness.NativeStatus, harness.NativeAttach, harness.NativeQuiesce,
		harness.NativeConfigure, harness.NativeGate, opClaudeHook:
		return control(op)
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
	s := &server{config: c, deadline: c.Deadline, state: harness.NativePrepared, threadID: c.ThreadID,
		done: make(chan struct{})}
	if bootstrap {
		s.state = harness.NativeUnconfigured
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
			s.stop(causeTerminated)
		case <-nativepty.AtDeadline(c.Deadline, s.done):
			s.stop(causeDeadline)
		case <-s.done:
		}
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

// exitCause is why the harness ended. It is decided once, where the decision
// is made: by the exit watcher for a harness that ended by itself, by stop for
// one the supervisor ends, and by quiesce for a graceful stop.
type exitCause int

const (
	causeNone exitCause = iota
	causeSelf
	causeQuiesced
	causeDeadline
	causeTerminated
	causeFailed
)

// reported is the cause an exit report carries; a graceful stop has none.
func (c exitCause) reported() (harness.NativeExitCause, bool) {
	switch c {
	case causeSelf:
		return harness.NativeExitSelf, true
	case causeDeadline:
		return harness.NativeExitDeadline, true
	case causeTerminated:
		return harness.NativeExitTerminated, true
	case causeFailed:
		return harness.NativeExitFailed, true
	}
	return "", false
}

// forced reports a cause under which the supervisor killed the harness.
func (c exitCause) forced() bool {
	return c == causeDeadline || c == causeTerminated || c == causeFailed
}

// decide records why the harness ended unless that is already decided. The
// caller holds s.mu.
func (s *server) decide(c exitCause) {
	if s.cause == causeNone {
		s.cause = c
	}
}

// end settles, once, that the harness has ended with code. The supervisor's
// own exit code is the harness's, but never 0 after a forced stop or an
// unconfirmed process-group stop. The caller holds s.mu and has decided the
// cause.
func (s *server) end(code int, waitErr error) {
	if s.ended {
		return
	}
	s.ended, s.state, s.harnessCode, s.exitCode = true, harness.NativeStopped, code, code
	if code == 0 && (waitErr != nil || s.cause.forced()) {
		s.exitCode = 1
	}
}

// conclude writes the exit report, unless a graceful stop ended the harness,
// and then closes done, after which the supervisor exits. It runs once.
func (s *server) conclude() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		n, cause, code := s.native, s.cause, s.harnessCode
		s.mu.Unlock()
		if reported, ok := cause.reported(); ok && n != nil {
			s.reportExit(n, reported, code)
		}
		close(s.done)
	})
}

// stop ends the harness for cause, unless its end is already decided.
func (s *server) stop(cause exitCause) {
	s.mu.Lock()
	n, login := s.native, s.login
	s.decide(cause)
	s.state = harness.NativeStopped
	s.mu.Unlock()
	var exit nativepty.Exit
	var err error
	if n != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		exit, err = n.Stop(ctx)
		login.stop()
	}
	s.mu.Lock()
	s.end(exit.Code, err)
	s.mu.Unlock()
	s.conclude()
}

// watch settles the harness's exit, unless a graceful stop in flight owns it.
// An exit nobody decided is the harness's own, or the terminal supervisor's
// deadline.
func (s *server) watch(n *nativepty.Supervisor, cleanup func()) {
	<-n.Done()
	exit, err := n.Wait(context.Background())
	cleanup()
	s.mu.Lock()
	if s.quiescing {
		s.mu.Unlock()
		return
	}
	if exit.Reason == nativepty.Deadline {
		s.decide(causeDeadline)
	}
	s.decide(causeSelf)
	s.end(exit.Code, err)
	s.mu.Unlock()
	s.conclude()
}

// setQuiescing marks whether a graceful stop owns the harness's exit.
func (s *server) setQuiescing(on bool) {
	s.mu.Lock()
	s.quiescing = on
	s.mu.Unlock()
}
func (s *server) configure(c *config) response {
	s.action.Lock()
	defer s.action.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	r := response{Op: harness.NativeConfigure, State: s.state, ThreadID: s.threadID}
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
	if s.state != harness.NativeUnconfigured {
		old, _ := json.Marshal(s.config)
		newConfig, _ := json.Marshal(c)
		if string(old) != string(newConfig) {
			r.Error = "native configuration is immutable"
		}
		return r
	}
	s.config = *c
	s.threadID = c.ThreadID
	s.state = harness.NativePrepared
	r.State, r.ThreadID = s.state, s.threadID
	return r
}
func (s *server) activate() response {
	s.action.Lock()
	defer s.action.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	r := response{Op: harness.NativeActivate, State: s.state, ThreadID: s.threadID}
	if s.state == harness.NativeActive {
		r.PID = s.native.PID()
		return r
	}
	if s.state != harness.NativePrepared {
		r.Error = "native launch gate cannot reopen"
		return r
	}
	if !s.config.Deadline.After(time.Now()) {
		r.Error = "native deadline expired"
		return r
	}
	// The forwarder puts the current projected token on each gateway CONNECT,
	// so the session outlives any one token (internal/nativeproxy).
	forwarder, proxyURL, err := startGatewayForwarder(s.config.Gateway)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	launched := false
	defer func() {
		if !launched {
			closeForwarder(forwarder)
		}
	}()
	env, err := s.config.Environment(proxyURL)
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
	adapter, err := adapterFor(s.config.Harness)
	if err != nil {
		r.Error = err.Error()
		return r
	}
	env = append(env, "HOME="+agentHome)
	if s.config.Task != "" && s.threadID != "" {
		_, baseline, err := observeTurn(nativeHome, s.threadID, s.config.InstructionDigest, s.config.RolloutMaxBytes)
		if err != nil {
			r.Error = "initial task baseline is unavailable: " + err.Error()
			return r
		}
		s.taskBaseline = baseline
	}
	// The sign-in comes first: a harness that keeps other settings in its
	// login file, such as Copilot's config.json, has them merged in.
	login, err := startLoginSync(s.config)
	if err != nil {
		r.Error = "harness sign-in is unavailable: " + err.Error()
		return r
	}
	s.login = login
	thread := s.threadID
	if thread == "" && adapter.namesSession {
		thread = uuid.NewString()
		s.launchID = thread
	}
	args, env, err := adapter.prepare(s.config, thread, env)
	if err != nil {
		login.stop()
		r.Error = err.Error()
		return r
	}
	n, err := nativepty.Start(nativepty.Launch{Executable: adapter.executable, Args: args, Env: env,
		Dir: workspace, Deadline: s.config.Deadline, Size: nativepty.Size{Rows: 40, Columns: 140}})
	if err != nil {
		login.stop()
		r.Error = err.Error()
		return r
	}
	s.native, s.state = n, harness.NativeActive
	r.PID, r.State = n.PID(), s.state
	launched = true
	go s.watch(n, func() {
		login.stop()
		closeForwarder(forwarder)
	})
	return r
}

// reportExit writes, for a harness that ended without a graceful stop, why it
// ended, its exit code, the last lines of its terminal and the thread of a
// completed turn, so the placement can explain the exit and keep a finished
// conversation.
func (s *server) reportExit(n *nativepty.Supervisor, cause harness.NativeExitCause, code int) {
	report := harness.NativeExitReport{Code: code, Cause: cause,
		Output: harness.NativeExitOutput(n.Tail(16<<10), 20)}
	s.mu.Lock()
	report.Thread = s.threadID
	s.mu.Unlock()
	if report.Thread == "" {
		if id, _, err := s.observeNativeTurn(); err == nil {
			report.Thread = id
		}
	}
	data, err := json.Marshal(report)
	if err == nil {
		out := s.reports
		if out == nil {
			out = os.Stdout
		}
		_, _ = fmt.Fprintln(out, harness.NativeExitReportPrefix+string(data))
	}
}
func (s *server) status() response {
	s.mu.Lock()
	state := s.state
	id := s.threadID
	n := s.native
	kind := s.config.Harness
	s.mu.Unlock()
	if id == "" && (state == harness.NativeActive || state == harness.NativeStopping) {
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
	r := response{Op: harness.NativeStatus, State: state, ThreadID: id}
	if id == "" && state == harness.NativeStopping {
		r.Turnless = s.isTurnless()
		if !r.Turnless {
			r.Waiting = s.waitingFor()
		}
	}
	if adapter, err := adapterFor(kind); err == nil && adapter.completion != nil {
		r.Completion = adapter.completion(s)
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
	if s.state != harness.NativeActive &&
		s.state != harness.NativeStopping {
		r := response{Op: harness.NativeGate, State: s.state, Error: inactiveError}
		s.mu.Unlock()
		return r
	}
	s.state = harness.NativeStopping
	s.markGated()
	thread := s.threadID
	s.mu.Unlock()
	n.GateInput()
	return response{Op: harness.NativeGate, State: harness.NativeStopping, ThreadID: thread}
}
func (s *server) quiesce() response {
	s.action.Lock()
	defer s.action.Unlock()
	s.mu.Lock()
	n := s.native
	if s.state != harness.NativeActive &&
		s.state != harness.NativeStopping {
		r := response{Op: harness.NativeQuiesce, State: s.state, Error: inactiveError}
		s.mu.Unlock()
		return r
	}
	s.state = harness.NativeStopping
	s.markGated()
	s.mu.Unlock()
	n.GateInput()
	ctx, cancel := context.WithDeadline(context.Background(), s.deadline)
	defer cancel()
	timer := time.NewTicker(100 * time.Millisecond)
	defer timer.Stop()
	bound := time.NewTimer(quiesceWait)
	defer bound.Stop()
	var id, turn string
	for {
		if s.isTurnless() {
			return s.stopTurnless(ctx, n)
		}
		var err error
		id, turn, err = s.observeNativeTurn()
		if err == nil {
			break
		}
		select {
		case <-ctx.Done():
			s.stop(causeDeadline)
			return response{Op: harness.NativeQuiesce, State: harness.NativeStopping,
				Error: "no completed safe native turn before deadline"}
		case <-n.Done():
			// No graceful stop owns this exit, so the exit watcher settles
			// and reports it as the harness's own.
			return response{Op: harness.NativeQuiesce, State: harness.NativeStopping,
				Error: "native process stopped before completed turn observation"}
		case <-bound.C:
			// Report what the stop waits for and leave the harness running
			// with input gated; a repeated quiesce waits again.
			return response{Op: harness.NativeQuiesce, State: harness.NativeStopping, Waiting: s.waitingFor()}
		case <-timer.C:
		}
	}
	adapter, err := adapterFor(s.config.Harness)
	if err != nil {
		s.stop(causeFailed)
		return response{Op: harness.NativeQuiesce, State: harness.NativeStopping, Error: err.Error()}
	}
	s.setQuiescing(true)
	exit, err := adapter.finish(n, ctx)
	s.mu.Lock()
	login := s.login
	s.mu.Unlock()
	// Save the harness's last sign-in change before the supervisor exits.
	login.stop()
	if err != nil {
		s.setQuiescing(false)
		s.stop(causeFailed)
		return response{Op: harness.NativeQuiesce, State: harness.NativeStopping, Error: err.Error()}
	}
	code, problem := adapter.settle(exit)
	s.endGracefully(exit, code, id)
	return response{Op: harness.NativeQuiesce, State: harness.NativeStopped, ThreadID: id, TurnID: turn,
		Exit: &harness.NativeExit{Code: exit.Code, Reason: string(exit.Reason)}, Error: problem}
}

// endGracefully settles a harness a graceful stop ended, with the
// supervisor's own exit code and the conversation it kept. A cause decided
// earlier, such as a SIGTERM that raced the stop, stays.
func (s *server) endGracefully(exit nativepty.Exit, code int, thread string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.quiescing = false
	s.decide(causeQuiesced)
	if thread != "" {
		s.threadID = thread
	}
	if !s.ended {
		s.ended, s.state, s.harnessCode, s.exitCode = true, harness.NativeStopped, exit.Code, code
	}
}

// waitingFor describes, in fixed words, what a graceful stop waits for.
func (s *server) waitingFor() string {
	s.mu.Lock()
	kind := s.config.Harness
	s.mu.Unlock()
	if kind == harness.NativeCopilot {
		if _, _, err := s.observeCopilotTurn(); errors.Is(err, os.ErrNotExist) {
			return "Copilot to start its first turn"
		}
		return "the current Copilot turn to complete"
	}
	if kind != harness.NativeClaude {
		if !codexJournalExists(nativeHome) {
			return "Codex to start its first turn"
		}
		return "the current Codex turn to complete"
	}
	switch s.claudeCompletion().Observation {
	case pendingTools:
		return "a running Claude Code tool to finish"
	case inputChanged:
		return "terminal input sent after the last Claude Code turn to settle"
	case "identity_unavailable":
		return "Claude Code to report its conversation"
	case "lifecycle_incomplete":
		return "the current Claude Code turn to finish"
	case "settled":
		return "Claude Code to stop"
	}
	return "Claude Code's transcript to settle"
}

// inputSettled reports that gated input has been quiet long enough for any
// prompt it submitted to have reported itself. The caller holds s.mu.
func (s *server) inputSettled() bool {
	last := s.lastInputAt
	if s.claude.lastHook.After(last) {
		last = s.claude.lastHook
	}
	return s.state == harness.NativeStopping && !s.gatedAt.IsZero() && time.Since(last) >= inputSettle
}

// inputSettle is how long gated input and lifecycle hooks must stay quiet
// before input accepted after the last completed turn no longer blocks a stop.
// A prompt submitted by that input, or queued during the turn, reports
// UserPromptSubmit well within it.
var inputSettle = 3 * time.Second

// markGated records when input was first gated. The caller holds s.mu.
func (s *server) markGated() {
	if s.gatedAt.IsZero() {
		s.gatedAt = time.Now()
	}
}

// isTurnless reports, and latches, that a gated first interactive visit has
// no native conversation to keep. A launch with a selected thread or a task
// always has one, so it never qualifies.
func (s *server) isTurnless() bool {
	s.mu.Lock()
	latched := s.turnless
	eligible := s.state == harness.NativeStopping && s.config.ThreadID == "" && s.threadID == "" &&
		s.config.Task == "" && !s.gatedAt.IsZero() && time.Since(s.gatedAt) >= turnlessSettle
	kind := s.config.Harness
	s.mu.Unlock()
	if latched || !eligible {
		return latched
	}
	adapter, err := adapterFor(kind)
	if err != nil || adapter.turnless == nil || !adapter.turnless(s) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.threadID != "" {
		return false
	}
	s.turnless = true
	return true
}

// stopTurnless ends a turnless first visit. There is no native conversation
// to preserve, so the supervisor stops the process group itself rather than
// typing a quit command into whatever the harness shows.
func (s *server) stopTurnless(ctx context.Context, n *nativepty.Supervisor) response {
	s.setQuiescing(true)
	exit, err := n.Stop(ctx)
	if err != nil {
		s.setQuiescing(false)
		s.stop(causeFailed)
		return response{Op: harness.NativeQuiesce, State: harness.NativeStopping, Error: err.Error()}
	}
	s.mu.Lock()
	login := s.login
	s.mu.Unlock()
	login.stop()
	s.endGracefully(exit, 0, "")
	return response{Op: harness.NativeQuiesce, State: harness.NativeStopped, Turnless: true,
		Exit: &harness.NativeExit{Code: exit.Code, Reason: string(exit.Reason)}}
}

func (s *server) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 4096), harness.NativeConfigureMaxBytes+64<<10)
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
	case harness.NativeConfigure:
		_ = encoder.Encode(s.configure(q.Config))
	case harness.NativeGate:
		_ = encoder.Encode(s.gate())
	case harness.NativeActivate:
		_ = encoder.Encode(s.activate())
	case harness.NativeStatus:
		_ = encoder.Encode(s.status())
	case harness.NativeQuiesce:
		_ = conn.SetDeadline(s.deadline)
		r := s.quiesce()
		_ = encoder.Encode(r)
		if r.State == harness.NativeStopped {
			s.conclude()
		}
	case harness.NativeAttach:
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
	if n == nil || state != harness.NativeActive {
		_ = encoder.Encode(response{Op: harness.NativeAttach, Error: inactiveError})
		return
	}
	a, err := n.Attach(cursor)
	if err != nil {
		_ = encoder.Encode(response{Op: harness.NativeAttach, Error: err.Error()})
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
	if emit(response{Op: harness.NativeAttach, PID: n.PID(), State: state}) != nil {
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
			if emit(response{Op: opOutput, Data: out.Data, Cursor: out.Next, Gap: out.Gap}) != nil || out.Exited {
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
			case opResize:
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
func control(op harness.NativeOp) error {
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
	if op == harness.NativeConfigure {
		var c config
		decoder := json.NewDecoder(io.LimitReader(os.Stdin, harness.NativeConfigureMaxBytes))
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
	if op == harness.NativeAttach {
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
