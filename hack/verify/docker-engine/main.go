// Command docker-engine proves local engine incarnation and termination controls.
// It is a lifecycle prerequisite, not a portable-session or gateway proof.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/andrewmccall/sproozi/internal/runtime/docker"
)

func main() {
	socket := flag.String("socket", "", "administrator-selected local Docker Unix socket")
	image := flag.String("image", "sproozi-kind/clients:acceptance", "installed trusted fixture image")
	flag.Parse()
	if err := run(*socket, *image); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "Docker lifecycle proof failed:", err)
		os.Exit(1)
	}
}

func run(socket, image string) error {
	binary, err := exec.LookPath("docker")
	if err != nil {
		return err
	}
	binary, err = filepath.Abs(binary)
	if err != nil {
		return err
	}
	private, err := os.MkdirTemp("", "sproozi-docker-control-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(private) }()
	engine, err := docker.NewEngine(binary, socket, private)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	imageID, err := engine.ImageID(ctx, image)
	if err != nil {
		return err
	}
	instance, err := engine.CreateIsolated(ctx, "sproozi-lifecycle-"+fmt.Sprint(time.Now().UnixNano()), imageID,
		[]string{"/bin/sh", "-c",
			"test ! -e /var/run/docker.sock && test ! -e /root/.codex/auth.json && exec sleep 300"},
		256<<20, 500, 64)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 20*time.Second)
		defer done()
		_ = engine.Stop(cleanup, instance)
		_ = engine.RemoveStopped(cleanup, instance)
	}()
	if err = engine.Start(ctx, instance); err != nil {
		return err
	}
	running, err := engine.Observe(ctx, instance)
	if err != nil || !running.Running || running.PID <= 0 {
		return fmt.Errorf("native fixture did not stay running: %v", err)
	}
	wrong := instance
	wrong.Execution = "another-execution"
	if _, err = engine.Observe(ctx, wrong); err == nil {
		return fmt.Errorf("accepted another execution's container")
	}
	if err = engine.Stop(ctx, instance); err != nil {
		return err
	}
	stopped, err := engine.Observe(ctx, instance)
	if err != nil || stopped.Running || stopped.PID != 0 || stopped.Status != "exited" {
		return fmt.Errorf("native container termination unavailable: %v", err)
	}
	if err = engine.Start(ctx, instance); err == nil {
		return fmt.Errorf("reopened a terminal Docker placement")
	}
	return json.NewEncoder(os.Stdout).Encode(struct {
		Passed  bool               `json:"passed"`
		Image   string             `json:"imageId"`
		Running docker.Observation `json:"running"`
		Stopped docker.Observation `json:"stopped"`
		Limits  string             `json:"limitations"`
	}{
		true, imageID, running, stopped,
		"Lifecycle prerequisite only; no gateway, storage, conversation or placement transfer",
	})
}
