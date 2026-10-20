//go:build !windows

package localsetup

import (
	"os/exec"
	"syscall"
)

// detach starts the service in its own session so terminal signals and
// closing the terminal do not reach it.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

func terminate(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

func alive(pid int) bool { return pid > 0 && syscall.Kill(pid, 0) == nil }
