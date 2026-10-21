package home

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/proxytransport"
	"github.com/andrewmccall/sproozi/internal/session"
	"github.com/andrewmccall/sproozi/internal/work"
	"github.com/andrewmccall/sproozi/internal/workspace"
)

const (
	ActionStart    = "start"
	ActionAttach   = "attach"
	ActionMove     = "move"
	ActionStatus   = "status"
	ActionCancel   = "cancel"
	ActionRecover  = "recover"
	ActionPull     = "pull"
	ActionResume   = "resume"
	ActionStop     = "stop"
	ActionSessions = "sessions"
	controlLimit   = 32 << 10
	responseLimit  = 2 << 20
)

type Request struct {
	Action         string     `json:"action"`
	ID             string     `json:"id"`
	OperationID    string     `json:"operationId,omitempty"`
	Destination    string     `json:"destination,omitempty"`
	Workspace      string     `json:"workspace,omitempty"`
	IncludeIgnored []string   `json:"includeIgnored,omitempty"`
	Source         string     `json:"source,omitempty"`
	Project        project.ID `json:"project,omitempty"`
	// Harness selects a configured local harness for start; empty selects
	// the primary local placement's harness.
	Harness harness.NativeKind `json:"harness,omitempty"`
	// HarnessArgs are the user's own arguments for the stock harness of a
	// start or resume, appended to the harness command line.
	HarnessArgs []string `json:"harnessArgs,omitempty"`
}
type Response struct {
	Status   *Status  `json:"status,omitempty"`
	Error    string   `json:"error,omitempty"`
	Sessions []Status `json:"sessions,omitempty"`
	// UnknownSession says Error reports that home retains no such session.
	UnknownSession bool `json:"unknownSession,omitempty"`
	// Rejected says Error reports a request home refused before recording
	// anything, so repeating it unchanged cannot help.
	Rejected bool `json:"rejected,omitempty"`
}

// ErrRejected matches a reply to a request home refused before recording it.
var ErrRejected = errors.New("home: request rejected")

type rejectedError string

func (e rejectedError) Error() string        { return string(e) }
func (e rejectedError) Is(target error) bool { return target == ErrRejected }

// ErrUnknownSession matches a reply that home retains no session with the
// requested UUID, so the client knows a retry cannot help.
var ErrUnknownSession = errors.New("docker (home): no such session")

type unknownSessionError string

func (e unknownSessionError) Error() string        { return string(e) }
func (e unknownSessionError) Is(target error) bool { return target == ErrUnknownSession }

// replyError turns a reply's error text back into an error.
func replyError(r Response) error {
	switch {
	case r.Error == "":
		return nil
	case r.UnknownSession:
		return unknownSessionError(r.Error)
	case r.Rejected:
		return rejectedError(r.Error)
	}
	return errors.New(r.Error)
}

// unknownSession replaces a missing-file error with the actionable reply when
// home retains neither a pulled session nor an execution with this UUID. Start
// creates the UUID and pull imports it, so neither reports it as unknown.
func (s *Service) unknownSession(q Request, err error) (Response, bool) {
	if q.Action == ActionStart || q.Action == ActionPull || !work.CanonicalID(q.ID) ||
		err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Response{}, false
	}
	if _, catalogErr := s.readCatalog(q.ID); !errors.Is(catalogErr, fs.ErrNotExist) {
		return Response{}, false
	}
	if _, inspectErr := s.coordinator.Inspect(q.ID); !errors.Is(inspectErr, fs.ErrNotExist) {
		return Response{}, false
	}
	return Response{UnknownSession: true, Error: fmt.Sprintf(
		"docker (home): no session %s; run \"sproozi sessions\" to list sessions", q.ID)}, true
}

// SocketPath returns the private local control address for a home directory.
func SocketPath(directory string) string { return socketPath(directory) }

// SocketPathProblem states why a control socket path given explicitly, such
// as with --socket, is too long for this platform, or returns "".
func SocketPathProblem(path string) string { return controlPathLength(path) }

// Serve starts the home gateway before the private control socket. Disconnecting
// a client does not cancel its durable start/move intent or the native process.
// A separate validated cancellation request interrupts graceful work through the
// service's operation control before waiting for its journal mutation mutex.
// Daemon shutdown releases ownership; it does not claim allocations stopped.
func (s *Service) Serve(ctx context.Context) error {
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, len(s.stacks))
	addresses := make([]string, 0, len(s.stacks))
	for _, stack := range s.stacks {
		server := stack.server
		server.BaseContext = func(net.Listener) context.Context { return run }
		gatewayListener, err := listenGateway(server.Addr)
		if err != nil {
			return err
		}
		if gateway, ok := server.Handler.(*proxytransport.Gateway); ok {
			gatewayListener = gateway.LimitListener(gatewayListener)
		}
		addresses = append(addresses, gatewayListener.Addr().String())
		go func() { done <- server.Serve(tls.NewListener(gatewayListener, server.TLSConfig)) }()
		defer func() { _ = server.Close() }()
	}
	go s.certs.run(run)
	path := SocketPath(s.config.Directory)
	listener, err := listenControl(path)
	if errors.Is(err, errControlListening) {
		return fmt.Errorf("home: control socket %s: another process is listening there; stop it first "+
			"(a home daemon for directory %s, or another program using that path)", path, s.config.Directory)
	} else if addressInUse(err) {
		return fmt.Errorf("home: control socket %s: address already in use; stop the other home daemon for "+
			"directory %s", path, s.config.Directory)
	} else if err != nil {
		return fmt.Errorf("home: control socket %s: %w", path, err)
	}
	defer func() { _ = listener.Close() }()
	if s.log != nil {
		_, _ = fmt.Fprintf(s.log, "Started home daemon: control socket %s, gateway %s\n", path, strings.Join(addresses, ", "))
	}
	var workers sync.WaitGroup
	var connections sync.Map
	shutdown := make(chan struct{})
	defer close(shutdown)
	go func() {
		select {
		case <-run.Done():
		case <-shutdown:
		}
		_ = listener.Close()
		connections.Range(func(key, value any) bool { _ = key.(net.Conn).Close(); return true })
	}()
	go func() {
		select {
		case <-done:
			cancel()
		case <-shutdown:
		}
	}()
	slots := make(chan struct{}, 16)
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			cancel()
			workers.Wait()
			if ctx.Err() != nil {
				if s.log != nil {
					_, _ = fmt.Fprintln(s.log, "Stopped home daemon")
				}
				return nil
			}
			return errors.New("home: gateway or control listener stopped")
		}
		select {
		case slots <- struct{}{}:
		default:
			_ = conn.Close()
			continue
		}
		connections.Store(conn, struct{}{})
		workers.Go(func() { defer func() { connections.Delete(conn); <-slots; _ = conn.Close() }(); s.handle(run, conn) })
	}
}

// addressInUse reports a bind refused because another listener holds the
// address. Windows reports WSAEADDRINUSE (10048) rather than EADDRINUSE.
func listenGateway(address string) (net.Listener, error) {
	listener, err := net.Listen("tcp4", address)
	if addressInUse(err) {
		return nil, fmt.Errorf("home: gateway.listen %q: address already in use; stop the other process or choose "+
			"another port (and local.gatewayPort)", address)
	} else if err != nil {
		return nil, fmt.Errorf("home: gateway.listen %q: %w", address, err)
	}
	return listener, nil
}

// CheckListen reports a gateway.listen address that serve could not bind,
// before Open creates or records anything under the home directory. Serve
// binds again and still reports a port taken in between. A port held by a
// home daemon that answers on this directory's control socket is reported as
// that daemon, so a second serve is not told to change ports.
func CheckListen(ctx context.Context, c Config) error {
	addresses := []string{c.Gateway.Listen}
	if stacks, err := localConfigs(c); err == nil {
		addresses = addresses[:0]
		for _, stack := range stacks {
			addresses = append(addresses, stack.gateway.Listen)
		}
	}
	for _, address := range addresses {
		listener, err := listenGateway(address)
		if err != nil {
			if daemonRunning(ctx, c.Directory) {
				return anotherDaemon(c.Directory)
			}
			return err
		}
		if err = listener.Close(); err != nil {
			return err
		}
	}
	return nil
}

// daemonRunning dials the directory's control socket without creating
// anything; only a live home daemon answers it.
func daemonRunning(ctx context.Context, directory string) bool {
	conn, err := dialControl(ctx, socketPath(directory))
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func anotherDaemon(directory string) error {
	return fmt.Errorf("home: another home daemon is using directory %s; stop it first (\"sproozi serve\" holds it until it exits)", directory)
}

func addressInUse(err error) bool {
	var errno syscall.Errno
	return errors.Is(err, syscall.EADDRINUSE) || errors.As(err, &errno) && errno == 10048
}

func readLine(reader *bufio.Reader, max int) ([]byte, error) {
	var line []byte
	for {
		part, err := reader.ReadSlice('\n')
		if len(line)+len(part) > max {
			return nil, errors.New("home: control frame exceeds bound")
		}
		line = append(line, part...)
		if err == nil {
			return line, nil
		}
		if err != bufio.ErrBufferFull {
			return nil, err
		}
	}
}
func (s *Service) handle(ctx context.Context, conn net.Conn) {
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(conn)
	data, err := readLine(reader, controlLimit)
	var q Request
	if err == nil {
		err = decode(data, &q)
	}
	if err == nil {
		err = validateRequest(q)
	}
	_ = conn.SetReadDeadline(time.Time{})
	encoder := json.NewEncoder(conn)
	if err != nil {
		_ = encoder.Encode(Response{Error: "home: invalid control request", Rejected: true})
		return
	}
	if q.Action == ActionAttach {
		if unknown, ok := s.unknownSession(q, nil); ok {
			_ = encoder.Encode(unknown)
			return
		}
		if err = encoder.Encode(Response{}); err != nil {
			return
		}
		if err = s.Attach(ctx, q.ID, reader, conn); err != nil {
			_ = encoder.Encode(struct {
				Error string `json:"error"`
			}{Error: "home: native attachment unavailable: " + err.Error()})
		}
		return
	}
	var status Status
	var sessions []Status
	switch q.Action {
	case ActionStart:
		status, err = s.Start(ctx, q.ID, q.OperationID, q.Workspace, q.Harness, q.IncludeIgnored, q.HarnessArgs)
	case ActionMove:
		status, err = s.Move(ctx, q.ID, q.OperationID, q.Destination)
	case ActionStatus:
		status, err = s.Status(q.ID)
	case ActionCancel:
		status, err = s.Cancel(ctx, q.ID, q.OperationID)
	case ActionRecover:
		status, err = s.Recover(ctx, q.ID)
	case ActionPull:
		status, err = s.Pull(ctx, q.ID, q.OperationID, q.Source)
	case ActionResume:
		status, err = s.Resume(ctx, q.ID, q.OperationID, q.HarnessArgs)
	case ActionStop:
		status, err = s.Stop(ctx, q.ID, q.OperationID)
	case ActionSessions:
		sessions, err = s.Sessions(work.SessionQuery{Project: q.Project})
	}
	response := Response{Sessions: sessions}
	if status.ID != "" {
		response.Status = &status
	}
	if err != nil {
		err = needsRecovery(err, q.ID)
		response.Error = err.Error()
		response.Rejected = errors.Is(err, ErrRejected)
		if unknown, ok := s.unknownSession(q, err); ok {
			response = unknown
		}
	}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = encoder.Encode(response)
}

// needsRecovery names the commands that unblock a session whose earlier
// operation is unfinished or cancelled. Repeating the refused request cannot
// help, so the reply is a rejection without a retry hint.
func needsRecovery(err error, id string) error {
	switch {
	case errors.Is(err, harness.ErrNativeTurnPending):
		return fmt.Errorf("%w; the session keeps running with its terminal input paused: repeat this command to keep "+
			"waiting, or run \"sproozi cancel %s\" to force-stop it", err, id)
	case errors.Is(err, session.ErrRecoveryRequired):
		return rejectedError(fmt.Sprintf("%v; run \"sproozi recover %s\" to finish it, or \"sproozi cancel %s\" "+
			"to revoke and force-stop its current execution", err, id, id))
	case errors.Is(err, session.ErrCancellationUnfinished):
		return rejectedError(fmt.Sprintf("%v; run \"sproozi recover %s\" to finish the cancellation", err, id))
	case errors.Is(err, session.ErrSessionCancelled):
		return rejectedError(fmt.Sprintf("session %s is cancelled; start a new session", id))
	}
	return err
}

func validateRequest(q Request) error {
	if err := validateCommonRequest(q); err != nil {
		return err
	}
	if q.Action == ActionSessions {
		if q.ID != "" || q.OperationID != "" || q.Workspace != "" || q.Destination != "" {
			return errors.New("home: unexpected session listing fields")
		}
		return nil
	}
	if !work.CanonicalID(q.ID) {
		return errors.New("home: invalid session UUID")
	}
	switch q.Action {
	case ActionStart:
		if !work.CanonicalID(q.OperationID) || !filepath.IsAbs(q.Workspace) || q.Destination != "" {
			return errors.New("home: invalid start")
		}
	case ActionMove:
		if !work.CanonicalID(q.OperationID) || q.Destination == "" || q.Workspace != "" {
			return errors.New("home: invalid move")
		}
	case ActionPull:
		if !work.CanonicalID(q.OperationID) || q.Source == "" || q.Workspace != "" || q.Destination != "" {
			return errors.New("home: invalid pull")
		}
	case ActionResume, ActionStop, ActionCancel:
		if !work.CanonicalID(q.OperationID) || q.Workspace != "" || q.Destination != "" {
			return errors.New("home: invalid execution intent")
		}
	case ActionAttach, ActionStatus, ActionRecover:
		if q.OperationID != "" || q.Workspace != "" || q.Destination != "" {
			return errors.New("home: unexpected control fields")
		}
	default:
		return errors.New("home: unsupported control action")
	}
	return nil
}

// errControlListening reports a control socket or pipe that a live process
// already serves; Serve never replaces it.
var errControlListening = errors.New("home: control socket has a live listener")

// ErrControlUnavailable reports that no home daemon is listening on the
// control address. It names the executor (docker) and its owner (home) as
// command output and --executor do.
var ErrControlUnavailable = errors.New("docker (home): no home daemon is listening on the private control socket")

func connect(ctx context.Context, path string, q Request) (net.Conn, *bufio.Reader, error) {
	if err := validateRequest(q); err != nil {
		return nil, nil, err
	}
	conn, err := dialControl(ctx, path)
	if err != nil {
		return nil, nil, err
	}
	if err = json.NewEncoder(conn).Encode(q); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	return conn, bufio.NewReader(conn), nil
}

func Call(ctx context.Context, path string, q Request) (Response, error) {
	var response Response
	conn, reader, err := connect(ctx, path, q)
	if err != nil {
		return response, err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	data, err := readLine(reader, responseLimit)
	if err != nil {
		return response, err
	}
	if err = decode(data, &response); err != nil {
		return response, err
	}
	return response, replyError(response)
}

var _ io.Closer = (*Service)(nil)

func validateCommonRequest(q Request) error {
	if q.Project != "" && (q.Action != ActionSessions || !project.ValidID(q.Project)) {
		return errors.New("home: project filters require sessions and a valid project ID")
	}

	if err := workspace.ValidateIgnoredPaths(q.IncludeIgnored); err != nil {
		return err
	}
	if q.Action != ActionStart && len(q.IncludeIgnored) != 0 {
		return errors.New("home: ignored file selection is start intent")
	}
	if q.Harness != "" && (q.Action != ActionStart || q.Harness.Validate() != nil) {
		return errors.New("home: harness selection applies only to start")
	}
	if q.Action != ActionPull && q.Source != "" {
		return errors.New("home: source is pull intent")
	}
	if len(q.HarnessArgs) != 0 && q.Action != ActionStart && q.Action != ActionResume {
		return errors.New("home: only start and resume take harness arguments")
	}
	return nil
}
