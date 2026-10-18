package home

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	"github.com/Microsoft/go-winio"
	"golang.org/x/sys/windows"
)

const controlPipePrefix = `\\.\pipe\`

func controlUser() (*windows.SID, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	return user.User.Sid.Copy()
}

func socketPath(directory string) string {
	// The public address helper cannot report errors. Empty means unavailable and
	// is rejected by both listener and dialer before any operating-system call.
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return ""
	}
	if canonical, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = canonical
	}
	user, err := controlUser()
	if err != nil {
		return ""
	}
	digest := sha256.Sum256([]byte(strings.ToLower(filepath.Clean(absolute)) + "\x00" + user.String()))
	return fmt.Sprintf(`%ssproozi-%x`, controlPipePrefix, digest)
}

func validControlPipe(path string) bool {
	if !strings.HasPrefix(path, controlPipePrefix) {
		return false
	}
	name := strings.TrimPrefix(path, controlPipePrefix)
	return name != "" && name != "." && name != ".." && len(name) <= 200 && !strings.ContainsAny(name, "\\/:\x00")
}

func listenControl(path string) (net.Listener, error) {
	if !validControlPipe(path) {
		return nil, errors.New("home: local control pipe required")
	}
	user, err := controlUser()
	if err != nil {
		return nil, err
	}
	// go-winio v0.6.4 creates the first instance with FILE_CREATE and always
	// rejects remote clients. Keep an explicit protected descriptor: defaults
	// admit users outside this home's ordinary-user trust boundary.
	descriptor := fmt.Sprintf("O:%sD:P(A;;GA;;;%s)(A;;GA;;;SY)", user, user)
	return winio.ListenPipe(path, &winio.PipeConfig{SecurityDescriptor: descriptor, InputBufferSize: controlLimit, OutputBufferSize: controlLimit})
}

func dialControl(ctx context.Context, path string) (net.Conn, error) {
	if !validControlPipe(path) {
		return nil, errors.New("home: local control pipe required")
	}
	user, err := controlUser()
	if err != nil {
		return nil, err
	}
	bounded, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// DialPipeContext requests anonymous impersonation, so a squatting server
	// cannot impersonate the client while its identity is being checked.
	conn, err := winio.DialPipeContext(bounded, path)
	if err != nil {
		return nil, err
	}
	if err = verifyControlServer(conn, user); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func verifyControlServer(conn net.Conn, user *windows.SID) error {
	file, ok := conn.(interface{ Fd() uintptr })
	if !ok {
		return errors.New("home: control pipe handle unavailable")
	}
	var pid uint32
	if err := windows.GetNamedPipeServerProcessId(windows.Handle(file.Fd()), &pid); err != nil {
		return err
	}
	process, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION|windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(process) }()
	var token windows.Token
	if err = windows.OpenProcessToken(process, windows.TOKEN_QUERY, &token); err != nil {
		return err
	}
	defer func() { _ = token.Close() }()
	owner, err := token.GetTokenUser()
	if err != nil {
		return err
	}
	if !owner.User.Sid.Equals(user) {
		return errors.New("home: control pipe belongs to another user")
	}
	// Hold the process handle while inspecting its token; an exited process is
	// not an authenticated live server, even if its PID can later be reused.
	state, err := windows.WaitForSingleObject(process, 0)
	if err != nil {
		return err
	}
	if state != uint32(windows.WAIT_TIMEOUT) {
		return errors.New("home: control pipe server exited")
	}
	return nil
}
