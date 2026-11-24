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

	"github.com/andrewmccall/sproozi/internal/deadline"
	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
	"github.com/google/uuid"
)

// socketPath is the supervisor's private control socket; tests replace it.
var socketPath = "/tmp/sproozi-native/control.sock"

const (
	configPath    = "/etc/sproozi/native/config.json"
	workspace     = harness.NativeSessionRoot + "/workspace"
	agentHome     = "/home/agent"
	inactiveError = "native execution is not active"
	// deadlineExpired refuses configure or activate after the deadline.
	deadlineExpired = "native deadline expired"
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
	config config
	// deadline is the execution's deadline, owned by deadline.go.
	deadline time.Time
	// expired raises the deadline once; deadlineRaised says it did.
	expired        sync.Once
	deadlineRaised bool
	mu             sync.Mutex
	action         sync.Mutex
	native         *nativepty.Supervisor
	// life is the supervisor's lifecycle; only apply changes it.
	life     lifecycle
	threadID string
	// launchID is the conversation a new visit's supervisor named itself,
	// for a harness that takes its ID at launch (Copilot).
	launchID string
	// login keeps the harness's sign-in in step with its login store.
	login    *loginSync
	done     chan struct{}
	stopOnce sync.Once
	// reports receives exit reports; nil is standard output.
	reports io.Writer
	// diagnostics receives diagnostic lines; nil is standard error.
	diagnostics     io.Writer
	claude          claudeLifecycle
	inputGeneration uint64
	// lastInputAt is when the last terminal write was accepted.
	lastInputAt time.Time
}

// quiesceReplyGrace bounds how long a stopped quiesce waits for its client
// to finish reading the reply before the supervisor concludes and exits.
var quiesceReplyGrace = 10 * time.Second

// quiesceWait bounds one quiesce request's wait for a completed turn.
var quiesceWait = harness.NativeQuiesceWait

// quitWait bounds the wait for a harness to exit once a graceful stop has
// asked it to, after its turn completed (ADR 0035). A set deadline that comes
// sooner ends the wait instead. Codex exits within seconds of /quit; the
// bound must fit, with quiesceWait, inside the five minutes the control
// client waits for the reply.
var quitWait = 30 * time.Second

// stopWait bounds stop's wait for the harness it kills at a terminate, a
// deadline or a failed stop. It is set only here; the supervisor reads no
// flag, file or variable that changes it, and only tests replace it.
var stopWait = 3 * time.Second

// forcedStopWait bounds the wait for a harness the supervisor killed after
// quitWait.
var forcedStopWait = 3 * time.Second

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
		until, err := deadline.Parse(args[3])
		if err != nil || until.Passed(time.Now()) {
			return errors.New("bootstrap requires a future absolute deadline or none")
		}
		return serve(config{Deadline: until.Time()}, true)
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
	s := newServer(c, bootstrap)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
			s.stop(evTerminate)
		case <-s.done:
		}
	}()
	go s.watchDeadline()
	go func() { <-s.done; _ = listener.Close() }()
	for {
		conn, err := listener.Accept()
		if err != nil {
			select {
			case <-s.done:
				s.mu.Lock()
				code := s.life.exitCode
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

// newServer is a supervisor configured with c, or bootstrapped to wait for
// its configuration.
func newServer(c config, bootstrap bool) *server {
	s := &server{config: c, deadline: c.Deadline, life: lifecycle{phase: phasePrepared}, threadID: c.ThreadID,
		done: make(chan struct{})}
	if bootstrap {
		s.life.phase = phaseUnconfigured
	}
	return s
}

// apply changes the lifecycle (lifecycle.go). The caller holds s.mu.
func (s *server) apply(c change) bool { return s.life.apply(c) }

// exitCause is why the harness ended. It is decided once, by the lifecycle
// event that forces or settles the end: the exit watcher's for a harness that
// ended by itself, stop's for one the supervisor ends, and the graceful
// stop's own.
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

// conclude writes the exit report, unless a graceful stop ended the harness,
// and then closes done, after which the supervisor exits. It runs once.
func (s *server) conclude() {
	s.stopOnce.Do(func() {
		s.mu.Lock()
		n, cause, code := s.native, s.life.cause, s.life.harnessCode
		s.mu.Unlock()
		if reported, ok := cause.reported(); ok && n != nil {
			s.reportExit(n, reported, code)
		}
		close(s.done)
	})
}

// stop ends the harness for e (terminate, deadline or fail), unless its end
// is already decided.
func (s *server) stop(e event) {
	s.mu.Lock()
	n, login := s.native, s.login
	s.apply(change{event: e})
	s.mu.Unlock()
	var exit nativepty.Exit
	var err error
	if n != nil {
		ctx, cancel := context.WithTimeout(context.Background(), stopWait)
		defer cancel()
		exit, err = n.Stop(ctx)
		login.stop()
	}
	s.mu.Lock()
	s.apply(change{event: evExited, exit: exit, err: err})
	s.mu.Unlock()
	s.conclude()
}

// watch settles the harness's exit, unless a graceful stop in flight owns it.
// An exit nobody forced is the harness's own.
func (s *server) watch(n *nativepty.Supervisor, cleanup func()) {
	<-n.Done()
	exit, err := n.Wait(context.Background())
	if exit.Reason != nativepty.NativeExit || exit.Code < 0 {
		// A signal ended it, perhaps while it rewrote its login file. The
		// supervisor's own kills were recorded before they were sent
		// (nativepty.Launch.BeforeKill); one from outside is seen only now.
		s.killing()
	}
	cleanup()
	s.mu.Lock()
	// The watcher concludes only an exit it settled itself. One a graceful
	// stop settled is concluded by the stop, after its reply was read.
	settled := s.life.phase != phaseEnded
	s.apply(change{event: evExited, exit: exit, err: err})
	settled = settled && s.life.phase == phaseEnded
	s.mu.Unlock()
	if settled {
		s.conclude()
	}
}

// finishing records that a graceful stop takes over the harness's exit.
func (s *server) finishing() {
	s.mu.Lock()
	s.apply(change{event: evFinish})
	s.mu.Unlock()
}
func (s *server) configure(c *config) response {
	s.action.Lock()
	defer s.action.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	r := response{Op: harness.NativeConfigure, State: s.life.phase.wire(), ThreadID: s.threadID}
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
	// A configured supervisor answers a configure as a replay. The contract's
	// own validation has already refused a deadline that passed.
	switch err := s.life.admit(evConfigure, s.pastDeadline()); {
	case errors.Is(err, errUndeclared):
		old, _ := json.Marshal(s.config)
		newConfig, _ := json.Marshal(c)
		if string(old) != string(newConfig) {
			r.Error = "native configuration is immutable"
		}
		return r
	case err != nil:
		r.Error = deadlineExpired
		return r
	}
	s.config = *c
	s.threadID = c.ThreadID
	s.apply(change{event: evConfigure})
	r.State, r.ThreadID = s.life.phase.wire(), s.threadID
	return r
}
func (s *server) activate() response {
	s.action.Lock()
	defer s.action.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	r := response{Op: harness.NativeActivate, State: s.life.phase.wire(), ThreadID: s.threadID}
	if s.life.phase == phaseActive {
		// A replay answers before admission.
		r.PID = s.native.PID()
		return r
	}
	switch err := s.life.admit(evActivate, s.pastDeadline()); {
	case errors.Is(err, errUndeclared):
		r.Error = "native launch gate cannot reopen"
		return r
	case err != nil:
		r.Error = deadlineExpired
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
	// The sign-in comes first: a harness that keeps other settings in its
	// login file, such as Copilot's config.json, has them merged in.
	login, err := startLogin(s.config)
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
	args, env, err := prepareLaunch(adapter, s.config, thread, env, login)
	if err != nil {
		login.stop()
		r.Error = err.Error()
		return r
	}
	// Preparing can take seconds, so the deadline is checked again at the
	// launch: the harness never starts after it.
	if s.pastDeadline() {
		login.stop()
		r.Error = launchPastDeadline
		return r
	}
	n, err := startTerminal(terminalLaunch(adapter.executable, args, env, login))
	if err != nil {
		login.stop()
		r.Error = err.Error()
		return r
	}
	s.native = n
	s.apply(change{event: evActivate})
	r.PID, r.State = n.PID(), s.life.phase.wire()
	launched = true
	go s.watch(n, func() {
		login.stop()
		closeForwarder(forwarder)
	})
	return r
}

// prepareLaunch runs the adapter's launch preparation, which may write the
// harness's login file (Copilot's workspace trust), and then tells the login
// sync that the supervisor's own writes are done, which starts its periodic
// sync. Until then no periodic sync can take that write for the harness's.
func prepareLaunch(adapter nativeAdapter, c config, thread string, env []string,
	login *loginSync) ([]string, []string, error) {
	args, env, err := adapter.prepare(c, thread, env)
	if err != nil {
		return nil, nil, err
	}
	login.settled()
	return args, env, nil
}

// terminalLaunch is the harness's terminal. Every kill its supervisor sends
// is recorded in the login sync first, so the harness's login file is no
// longer copied out once a kill may land (loginSync). It has no deadline of
// its own: the supervisor's deadline (deadline.go) stops it.
func terminalLaunch(executable string, args, env []string, login *loginSync) nativepty.Launch {
	return nativepty.Launch{Executable: executable, Args: args, Env: env, Dir: workspace,
		Size: nativepty.Size{Rows: 40, Columns: 140}, BeforeKill: login.forced}
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
	p := s.life.phase
	id := s.threadID
	n := s.native
	kind := s.config.Harness
	s.mu.Unlock()
	if id == "" && p.running() {
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
	r := response{Op: harness.NativeStatus, State: p.wire(), ThreadID: id}
	if id == "" && p.stopping() {
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
	if !s.gateInput() {
		r := response{Op: harness.NativeGate, State: s.life.phase.wire(), Error: inactiveError}
		s.mu.Unlock()
		return r
	}
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
	if !s.gateInput() {
		r := response{Op: harness.NativeQuiesce, State: s.life.phase.wire(), Error: inactiveError}
		s.mu.Unlock()
		return r
	}
	s.mu.Unlock()
	n.GateInput()
	timer := time.NewTicker(100 * time.Millisecond)
	defer timer.Stop()
	bound := time.NewTimer(quiesceWait)
	defer bound.Stop()
	var id, turn string
	for {
		if s.isTurnless() {
			return s.stopTurnless(n)
		}
		var err error
		id, turn, err = s.observeNativeTurn()
		if err == nil {
			break
		}
		if s.expireIfPassed() {
			return response{Op: harness.NativeQuiesce, State: harness.NativeStopping,
				Error: "no completed safe native turn before deadline"}
		}
		select {
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
		s.stop(evFail)
		return response{Op: harness.NativeQuiesce, State: harness.NativeStopping, Error: err.Error()}
	}
	s.finishing()
	exit, forced, err := s.finishTurn(adapter, n)
	if err == nil && forced != "" {
		// The harness state must still show the turn the stop observed.
		again, turnAgain, observeErr := s.observeNativeTurn()
		switch {
		case errors.Is(observeErr, errJournalRowIncomplete):
			// Capture, after the placement fences the container, leaves
			// the unfinished row out (ADR 0061).
			err = errors.New("the forced stop left a native journal row unfinished; " +
				"repeat the stop to complete it with the completed turns kept")
		case observeErr != nil || again != id || turnAgain != turn:
			err = errors.New("native turn changed before the forced stop")
		}
	}
	s.mu.Lock()
	login := s.login
	s.mu.Unlock()
	// Save the harness's last sign-in change before the supervisor exits.
	login.stop()
	if err != nil {
		if s.expireIfPassed() {
			return response{Op: harness.NativeQuiesce, State: harness.NativeStopping,
				Error: "native quit did not finish before deadline"}
		}
		s.stop(evFail)
		return response{Op: harness.NativeQuiesce, State: harness.NativeStopping, Error: err.Error()}
	}
	code, problem := adapter.settle(exit)
	if forced != "" {
		// Not a clean quit, but the turn completed and the harness is
		// stopped, so the stop stands. The supervisor exits 0, as after
		// Claude Code's kill: a placement that lost this reply and reads
		// only the exit code must not take the stop for a failed agent.
		code, problem = 0, ""
		s.diagnose("%s", forced)
	}
	// A harness that quits (Codex) and was killed by a forced stop before
	// its quit ended it did not stop gracefully. One the supervisor ends
	// with a kill after its completed turn (Claude Code, Copilot) did, whose
	// kill landed first: its stop stands and its turn is kept.
	if overtaken := s.endGracefully(exit, code, id); overtaken != "" && adapter.quits {
		return response{Op: harness.NativeQuiesce, State: harness.NativeStopping, Error: overtaken}
	}
	return response{Op: harness.NativeQuiesce, State: harness.NativeStopped, ThreadID: id, TurnID: turn,
		Exit: &harness.NativeExit{Code: exit.Code, Reason: string(exit.Reason)}, Error: problem}
}

// finishTurn ends a harness whose turn completed, with input gated. The wait
// for it to exit ends at a set deadline and in any case after quitWait, so a
// harness that ignores its quit cannot hold the action lock. One still running
// at quitWait, or whose full input buffer took no quit, is killed and waited
// for at most forcedStopWait, within the deadline. No work is in flight, so
// the stop stands. forced says why, only when the supervisor's kill ended the
// harness rather than its own exit. At the deadline the error stands, for the
// caller to report.
func (s *server) finishTurn(adapter nativeAdapter, n *nativepty.Supervisor) (nativepty.Exit, string, error) {
	until := s.until()
	ctx, cancel := context.WithDeadline(context.Background(), until.Bound(time.Now(), quitWait))
	defer cancel()
	exit, err := adapter.finish(n, ctx)
	var forced string
	switch {
	case err == nil || s.pastDeadline():
		return exit, "", err
	case errors.Is(err, nativepty.ErrInputBusy):
		forced = "Forced the harness to stop after its terminal input was full when it was asked to quit"
	case ctx.Err() == nil:
		return exit, "", err
	case adapter.quits:
		forced = fmt.Sprintf("Forced the harness to stop after it did not exit within %s of its quit", quitWait)
	default:
		// The finish was a kill already; only its confirmation was late.
		forced = fmt.Sprintf("Confirmed the harness stopped after its kill was not confirmed within %s", quitWait)
	}
	stopCtx, stopCancel := context.WithDeadline(context.Background(), until.Bound(time.Now(), forcedStopWait))
	defer stopCancel()
	exit, err = forceStop(n, stopCtx)
	if err != nil || exit.Reason != nativepty.ForcedStop {
		return exit, "", err
	}
	return exit, forced, nil
}

// forceStop kills the harness and waits for its exit; tests replace it.
var forceStop = (*nativepty.Supervisor).Stop

// startTerminal starts the harness's terminal; tests replace it.
var startTerminal = nativepty.Start

// startLogin starts the harness's sign-in sync; tests replace it.
var startLogin = startLoginSync

// launchPastDeadline is activate's reply when the deadline passed while it
// prepared the launch: the terminal's own refusal of a passed deadline,
// which gave this reply while the terminal was handed the deadline.
const launchPastDeadline = "native terminal requires an absolute executable/workspace, " +
	"explicit environment and a deadline not yet passed"

// killing records that a signal from outside the supervisor ended the
// harness, so its login file, which may be left half written, is no longer
// copied to the login store. The supervisor's own kills are recorded before
// they are sent (nativepty.Launch.BeforeKill).
func (s *server) killing() {
	s.mu.Lock()
	login := s.login
	s.mu.Unlock()
	login.forced()
}

// diagnose writes one line about the harness to the supervisor's standard
// error, or to diagnostics when set.
func (s *server) diagnose(format string, args ...any) {
	out := s.diagnostics
	if out == nil {
		out = os.Stderr
	}
	_, _ = fmt.Fprintf(out, format+"\n", args...)
}

// endGracefully settles a harness a graceful stop ended, with the
// supervisor's own exit code and the conversation it kept. A cause decided
// earlier, such as a SIGTERM that raced the stop, stays (evFinished). A stop
// such a cause overtook did not end the harness gracefully: overtaken says
// so, for the reply, which then agrees with the exit report and the
// supervisor's nonzero exit code.
func (s *server) endGracefully(exit nativepty.Exit, code int, thread string) (overtaken string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if thread != "" {
		s.threadID = thread
	}
	s.apply(change{event: evFinished, exit: exit, code: code})
	if reported, ok := s.life.cause.reported(); ok && s.life.cause.forced() {
		return "native stop was overtaken: the supervisor stopped the harness with cause " + string(reported)
	}
	return ""
}

// waitingFor describes, in fixed words, what a graceful stop waits for: its
// harness adapter's wait text. A kind without an adapter never stops
// gracefully, since configure refuses it and activate cannot launch it, so it
// has no wait text.
func (s *server) waitingFor() string {
	s.mu.Lock()
	kind := s.config.Harness
	s.mu.Unlock()
	adapter, err := adapterFor(kind)
	if err != nil {
		return ""
	}
	return adapter.waiting(s)
}

// inputSettled reports that gated input has been quiet long enough for any
// prompt it submitted to have reported itself. The caller holds s.mu.
func (s *server) inputSettled() bool {
	last := s.lastInputAt
	if s.claude.lastHook.After(last) {
		last = s.claude.lastHook
	}
	return s.life.phase.stopping() && !s.life.gatedAt.IsZero() && time.Since(last) >= inputSettle
}

// inputSettle is how long gated input and lifecycle hooks must stay quiet
// before input accepted after the last completed turn no longer blocks a stop.
// A prompt submitted by that input, or queued during the turn, reports
// UserPromptSubmit well within it.
var inputSettle = 3 * time.Second

// gateInput applies gate, recording for the login sync that a stop has
// begun, unless the lifecycle refuses it. The caller holds s.mu and gates the
// terminal itself.
func (s *server) gateInput() bool {
	if s.life.admit(evGate, s.pastDeadline()) != nil {
		return false
	}
	s.login.stopping()
	return s.apply(change{event: evGate, at: time.Now()})
}

// isTurnless reports, and latches, that a gated first interactive visit has
// no native conversation to keep. A launch with a selected thread or a task
// always has one, so it never qualifies.
func (s *server) isTurnless() bool {
	s.mu.Lock()
	latched := s.life.turnless
	eligible := s.life.phase.stopping() && s.config.ThreadID == "" && s.threadID == "" &&
		s.config.Task == "" && !s.life.gatedAt.IsZero() && time.Since(s.life.gatedAt) >= turnlessSettle
	kind := s.config.Harness
	s.mu.Unlock()
	if latched || !eligible {
		return latched
	}
	adapter, err := adapterFor(kind)
	if err != nil || !adapter.turnless(s) {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.threadID != "" {
		return false
	}
	return s.apply(change{event: evTurnless})
}

// stopTurnless ends a turnless first visit. There is no native conversation
// to preserve, so the supervisor stops the process group itself rather than
// typing a quit command into whatever the harness shows. The wait for it to
// exit has finishTurn's bound.
func (s *server) stopTurnless(n *nativepty.Supervisor) response {
	s.finishing()
	ctx, cancel := context.WithDeadline(context.Background(), s.until().Bound(time.Now(), quitWait))
	defer cancel()
	exit, err := forceStop(n, ctx)
	if err != nil {
		if !s.expireIfPassed() {
			s.stop(evFail)
		}
		return response{Op: harness.NativeQuiesce, State: harness.NativeStopping, Error: err.Error()}
	}
	s.mu.Lock()
	login := s.login
	s.mu.Unlock()
	login.stop()
	if overtaken := s.endGracefully(exit, 0, ""); overtaken != "" {
		return response{Op: harness.NativeQuiesce, State: harness.NativeStopping, Error: overtaken}
	}
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
		_ = conn.SetDeadline(s.until().Time())
		r := s.quiesce()
		_ = encoder.Encode(r)
		if r.State == harness.NativeStopped {
			// Concluding exits the supervisor, which on Kubernetes ends the
			// container and can cut the exec stream still carrying this reply.
			// The client closes only after writing the reply out, so wait for
			// that, within a bound.
			_ = conn.SetReadDeadline(time.Now().Add(quiesceReplyGrace))
			_, _ = io.Copy(io.Discard, conn)
			s.conclude()
		}
	case harness.NativeAttach:
		_ = conn.SetDeadline(s.until().Time())
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
	n, p := s.native, s.life.phase
	s.mu.Unlock()
	if n == nil || !p.takesInput() {
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
	if emit(response{Op: harness.NativeAttach, PID: n.PID(), State: p.wire()}) != nil {
		return
	}
	ctx, cancel := s.until().Context(context.Background())
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
