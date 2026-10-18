package home

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync"
	"time"

	"github.com/andrewmccall/sproozi/internal/project"
	"github.com/andrewmccall/sproozi/internal/proxytransport"
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
}
type Response struct {
	Status   *Status  `json:"status,omitempty"`
	Error    string   `json:"error,omitempty"`
	Sessions []Status `json:"sessions,omitempty"`
}

// SocketPath returns the private local control address for a home directory.
func SocketPath(directory string) string { return socketPath(directory) }

// Serve starts the home gateway before the private control socket. Disconnecting
// a client does not cancel its durable start/move intent or the native process.
// A separate validated cancellation request interrupts graceful work through the
// service's operation control before waiting for its journal mutation mutex.
// Daemon shutdown releases ownership; it does not claim allocations stopped.
func (s *Service) Serve(ctx context.Context) error {
	run, cancel := context.WithCancel(ctx)
	defer cancel()
	s.gateway.BaseContext = func(net.Listener) context.Context { return run }
	gatewayListener, err := net.Listen("tcp4", s.gateway.Addr)
	if err != nil {
		return err
	}
	if gateway, ok := s.gateway.Handler.(*proxytransport.Gateway); ok {
		gatewayListener = gateway.LimitListener(gatewayListener)
	}
	done := make(chan error, 1)
	go func() { done <- s.gateway.Serve(tls.NewListener(gatewayListener, s.gateway.TLSConfig)) }()
	defer func() { _ = s.gateway.Close() }()
	path := SocketPath(s.config.Directory)
	listener, err := listenControl(path)
	if err != nil {
		return err
	}
	defer func() { _ = listener.Close() }()
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
		_ = encoder.Encode(Response{Error: "home: invalid control request"})
		return
	}
	if q.Action == ActionAttach {
		if err = encoder.Encode(Response{}); err != nil {
			return
		}
		if err = s.Attach(ctx, q.ID, reader, conn); err != nil {
			_ = encoder.Encode(struct {
				Error string `json:"error"`
			}{Error: "home: native attachment unavailable"})
		}
		return
	}
	var status Status
	var sessions []Status
	switch q.Action {
	case ActionStart:
		status, err = s.Start(ctx, q.ID, q.OperationID, q.Workspace, q.IncludeIgnored)
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
		status, err = s.Resume(ctx, q.ID, q.OperationID)
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
		response.Error = err.Error()
	}
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_ = encoder.Encode(response)
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
	if response.Error != "" {
		return response, errors.New(response.Error)
	}
	return response, nil
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
	if q.Action != ActionPull && q.Source != "" {
		return errors.New("home: source is pull intent")
	}
	return nil
}
