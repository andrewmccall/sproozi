//go:build linux || darwin

package nativepty

import (
	"os"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

func openPTY() (*os.File, *os.File, error) {
	master, slave, err := pty.Open()
	if err != nil {
		return nil, nil, err
	}
	fd, err := unix.FcntlInt(master.Fd(), unix.F_DUPFD_CLOEXEC, 0)
	_ = master.Close()
	if err != nil {
		_ = slave.Close()
		return nil, nil, err
	}
	if err = unix.SetNonblock(fd, true); err != nil {
		_ = unix.Close(fd)
		_ = slave.Close()
		return nil, nil, err
	}
	// Construct os.File after O_NONBLOCK so Go registers a pollable descriptor.
	// Fd() must not be called on the returned master: it restores blocking mode.
	return os.NewFile(uintptr(fd), "native-pty-master"), slave, nil
}
