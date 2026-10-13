package main

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// Check the actual fixed executable before launch. The container image identity
// and the portable journal's claimed version cannot replace this observation.
func verifyNativeVersion(parent context.Context, kind harness.NativeKind,
	path, dir string, env []string, deadline time.Time) error {
	if limit := time.Now().Add(5 * time.Second); deadline.After(limit) {
		deadline = limit
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Dir, cmd.Env = dir, env
	cmd.WaitDelay = time.Second
	output := &versionOutput{}
	cmd.Stdout, cmd.Stderr = output, io.Discard
	if err := cmd.Run(); err != nil {
		return errors.New("native executable version observation failed")
	}
	version, err := harness.NativeVersion(kind)
	if err != nil {
		return err
	}
	expected := "codex-cli " + version
	if kind == harness.NativeClaude {
		expected = version + " (Claude Code)"
	}
	if strings.TrimSpace(output.value) != expected {
		return errors.New("native executable does not match the approved native version")
	}
	return nil
}

type versionOutput struct{ value string }

func (v *versionOutput) Write(data []byte) (int, error) {
	if len(v.value)+len(data) > 128 {
		return 0, errors.New("native version output exceeds limit")
	}
	v.value += string(data)
	return len(data), nil
}
