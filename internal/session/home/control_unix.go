//go:build linux || darwin

package home

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/andrewmccall/sproozi/internal/privatefs"
)

func socketPath(directory string) string { return filepath.Join(directory, "control.sock") }

func listenControl(path string) (net.Listener, error) {
	if err := privatefs.Check(filepath.Dir(path), true); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0077 != 0 {
			return nil, errors.New("home: unsafe stale control path")
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
	if err := privatefs.Check(filepath.Dir(path), true); err != nil {
		return nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("home: private control socket unavailable")
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	return dialer.DialContext(ctx, "unix", path)
}
