package localsetup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/privatefs"
	"github.com/andrewmccall/sproozi/internal/session/home"
)

// ServiceStatus describes the home service for one installation root.
type ServiceStatus struct {
	Running bool     `json:"running"`
	PID     int      `json:"pid,omitempty"`
	Socket  string   `json:"socket"`
	Log     string   `json:"log"`
	Active  []string `json:"activeSessions,omitempty"`
	Error   string   `json:"error,omitempty"`
}

// ServiceState asks the control socket for its session list; an answer means
// the gateway listeners and control socket are serving.
func ServiceState(ctx context.Context, l Layout, c home.Config) ServiceStatus {
	status := ServiceStatus{Socket: home.SocketPath(c.Directory), Log: l.ServiceLog(), PID: readPID(l)}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	response, err := home.Call(ctx, status.Socket, home.Request{Action: home.ActionSessions})
	if err != nil {
		status.Error = err.Error()
		return status
	}
	status.Running = true
	for _, s := range response.Sessions {
		if s.Ownership == "home" && running(s.Phase) {
			status.Active = append(status.Active, s.ID)
		}
	}
	return status
}

// running phases still have, or may have, a worker container.
func running(phase string) bool {
	switch phase {
	case "idle", "cancelled", "":
		return false
	}
	return true
}

// StartService runs `sproozi serve` detached from the terminal, appending to
// the private log, then waits until the control socket answers.
func StartService(ctx context.Context, l Layout, c home.Config, executable string, timeout time.Duration) (ServiceStatus, error) {
	if status := ServiceState(ctx, l, c); status.Running {
		return status, nil
	}
	if err := privatefs.EnsureDir(l.Logs()); err != nil {
		return ServiceStatus{}, err
	}
	log, err := os.OpenFile(l.ServiceLog(), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return ServiceStatus{}, err
	}
	defer func() { _ = log.Close() }()
	if err = privatefs.Check(l.ServiceLog(), false); err != nil {
		return ServiceStatus{}, err
	}
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return ServiceStatus{}, err
	}
	defer func() { _ = devnull.Close() }()
	cmd := exec.Command(executable, "serve", "--config", l.Config())
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, log, log
	cmd.Dir = l.Root
	detach(cmd)
	if err = cmd.Start(); err != nil {
		return ServiceStatus{}, fail("service", err, "")
	}
	pid := cmd.Process.Pid
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	if err = privatefs.Replace(l.Root, filepath.Base(l.ServicePID()), []byte(strconv.Itoa(pid)+"\n")); err != nil {
		return ServiceStatus{}, err
	}
	deadline := time.Now().Add(timeout)
	for {
		if status := ServiceState(ctx, l, c); status.Running {
			return status, nil
		}
		select {
		case err := <-exited:
			return ServiceStatus{}, fail("service", fmt.Errorf("home service exited during startup (%v): %s", err, logTail(l)), "read "+l.ServiceLog())
		case <-ctx.Done():
			return ServiceStatus{}, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return ServiceStatus{}, fail("service", errors.New("home service did not become ready"), "read "+l.ServiceLog())
		}
	}
}

// StopService stops the home service. Active executions keep running within
// their own deadlines unless the caller cancels them first; stopping the
// daemon is never evidence that a container stopped.
func StopService(ctx context.Context, l Layout, c home.Config, leaveRunning bool, timeout time.Duration) (ServiceStatus, error) {
	status := ServiceState(ctx, l, c)
	if !status.Running {
		return status, nil
	}
	if len(status.Active) > 0 && !leaveRunning {
		return status, fail("service", fmt.Errorf("%d session(s) are active: %s", len(status.Active), strings.Join(status.Active, ", ")),
			"stop or cancel them first, or pass --leave-running to keep them running without the service")
	}
	if status.PID <= 0 {
		return status, fail("service", errors.New("the running service was not started by sproozi service start"), "stop it where it was started")
	}
	if err := terminate(status.PID); err != nil {
		return status, fail("service", err, "")
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if after := ServiceState(ctx, l, c); !after.Running {
			_ = os.Remove(l.ServicePID())
			after.PID, after.Error = 0, ""
			return after, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return status, fail("service", errors.New("home service did not stop"), "")
}

func readPID(l Layout) int {
	data, err := privatefs.ReadFile(l.ServicePID(), 64)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || !alive(pid) {
		return 0
	}
	return pid
}

func logTail(l Layout) string {
	data, err := os.ReadFile(l.ServiceLog())
	if err != nil {
		return ""
	}
	if len(data) > 1024 {
		data = data[len(data)-1024:]
	}
	return strings.TrimSpace(string(data))
}
