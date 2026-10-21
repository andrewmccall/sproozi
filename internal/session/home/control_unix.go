//go:build linux || darwin

package home

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/andrewmccall/sproozi/internal/privatefs"
)

func socketPath(directory string) string { return filepath.Join(directory, "control.sock") }

// controlPathProblem explains a control socket path that exceeds the platform's
// sun_path limit (104 bytes on macOS, 108 on Linux, each including the NUL), or
// returns "". The bind would otherwise fail with a bare "invalid argument".
func controlPathProblem(path string) string {
	if problem := controlPathLength(path); problem != "" {
		return problem + "; set a shorter \"directory\" in config.json"
	}
	return ""
}

// controlPathLength states an over-long control socket path without a remedy,
// or returns "".
func controlPathLength(path string) string {
	limit := len(syscall.RawSockaddrUnix{}.Path) - 1
	if len(path) <= limit {
		return ""
	}
	return fmt.Sprintf("control socket path %q is %d bytes; this platform allows at most %d", path, len(path), limit)
}

func listenControl(path string) (net.Listener, error) {
	if err := privatefs.Check(filepath.Dir(path), true); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		// Serve prefixes these with the control socket path.
		if info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("the path exists and is not a socket (%s); remove it if no Sproozi service is "+
				"running, then start the service again", info.Mode().Type())
		}
		if info.Mode().Perm()&0077 != 0 {
			return nil, fmt.Errorf("the socket allows access by other accounts (mode %04o); remove it if no Sproozi "+
				"service is running, or run chmod 600 on it, then start the service again", info.Mode().Perm())
		}
		// Remove only a stale socket: one that answers belongs to a live process.
		if conn, dialErr := net.DialTimeout("unix", path, time.Second); dialErr == nil {
			_ = conn.Close()
			return nil, errControlListening
		}
		if err = os.Remove(path); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		_ = listener.Close()
		return nil, err
	}
	return listener, nil
}

func dialControl(ctx context.Context, path string) (net.Conn, error) {
	// No daemon can ever listen on an over-long path; say so rather than
	// suggesting "sproozi service run", which refuses the same directory.
	if problem := controlPathProblem(path); problem != "" {
		return nil, errors.New("home: " + problem)
	}
	// A missing socket means no daemon, whatever its directory's permissions.
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s does not exist", ErrControlUnavailable, path)
	}
	if err := privatefs.Check(filepath.Dir(path), true); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrControlUnavailable
		}
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0077 != 0 {
		return nil, ErrControlUnavailable
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	conn, err := dialer.DialContext(ctx, "unix", path)
	if errors.Is(err, syscall.ECONNREFUSED) {
		// A socket left by a stopped daemon refuses connections.
		return nil, ErrControlUnavailable
	}
	return conn, err
}
