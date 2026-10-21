//go:build windows

package localsetup

import (
	"errors"
	"os/exec"
	"syscall"
)

const createNewProcessGroup = 0x00000200

// detach starts the service in a new process group without a console.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup, HideWindow: true}
}

func terminate(int) error {
	return errors.New("automatic service stop is not supported on Windows; stop \"sproozi service run\" where it runs")
}

func alive(pid int) bool { return pid > 0 }
