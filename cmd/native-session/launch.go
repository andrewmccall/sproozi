package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
)

// nativeAdapter holds what differs between supported harnesses inside the
// supervisor. The lifecycle around it (gate, observe, stop) is shared.
type nativeAdapter struct {
	executable string
	// prepare installs launch-generated files for a native process resuming
	// thread ("" for a new conversation) and returns its arguments and
	// environment, extending env.
	prepare func(c config, thread string, env []string) ([]string, []string, error)
	// observe reports the selected native thread and its completed turn.
	observe func(s *server) (string, string, error)
	// completion reports lifecycle diagnostics when the harness has them.
	completion func(s *server) *harness.NativeCompletion
	// finish ends the process once its turn completed.
	finish func(n *nativepty.Supervisor, ctx context.Context) (nativepty.Exit, error)
	// settle maps that exit to the supervisor's exit code and, when the
	// process did not stop as expected, a problem to report.
	settle func(nativepty.Exit) (int, string)
}

var nativeAdapters = map[harness.NativeKind]nativeAdapter{
	harness.NativeCodex: {
		executable: executable,
		prepare:    prepareCodexLaunch,
		observe:    (*server).observeCodexTurn,
		// Codex quits itself after the completed turn and must exit cleanly.
		finish: (*nativepty.Supervisor).QuitAfterCompletedTurn,
		settle: func(exit nativepty.Exit) (int, string) {
			if exit.Code != 0 || exit.Reason != nativepty.NativeExit {
				code := exit.Code
				if code == 0 {
					code = 1
				}
				return code, "native quit did not exit normally"
			}
			return 0, ""
		},
	},
	harness.NativeClaude: {
		executable: claudeExecutable,
		prepare:    prepareClaudeLaunch,
		observe:    (*server).observeClaudeTurn,
		completion: (*server).claudeCompletion,
		// The observed lifecycle hooks proved completion; the supervisor
		// stops Claude itself.
		finish: (*nativepty.Supervisor).Stop,
		settle: func(exit nativepty.Exit) (int, string) {
			if exit.Reason != nativepty.ForcedStop {
				return 0, "native controlled stop did not finish"
			}
			return 0, ""
		},
	},
}

func adapterFor(kind harness.NativeKind) (nativeAdapter, error) {
	adapter, ok := nativeAdapters[kind]
	if !ok {
		return nativeAdapter{}, errors.New("unsupported native harness")
	}
	return adapter, nil
}

func prepareCodexLaunch(c config, thread string, env []string) ([]string, []string, error) {
	if err := installCodexProjectTrust(nativeHome); err != nil {
		return nil, nil, err
	}
	profile, err := loadProfile(c)
	if err != nil {
		return nil, nil, fmt.Errorf("profile: %w", err)
	}
	if err := installManagedCodex(nativeHome, profile); err != nil {
		return nil, nil, err
	}
	return codexArguments(c, thread), append(env, "CODEX_HOME="+nativeHome), nil
}

func codexArguments(c config, id string) []string {
	encoded, _ := json.Marshal(c.Instructions)
	args := []string{"--no-daemon", "--no-alt-screen", "--sandbox", "danger-full-access",
		"--ask-for-approval", "never", "--cd", workspace, "-c", "developer_instructions=" + string(encoded),
		"-c", "check_for_update_on_startup=false"}
	for _, option := range c.Options {
		args = append(args, "-c", option)
	}
	if c.ClientConfig != nil {
		for _, option := range c.ClientConfig.CodexOptions() {
			args = append(args, "-c", option)
		}
	}
	if c.Model != "" {
		args = append(args, "--model", c.Model)
	}
	if id != "" {
		args = append(args, "resume", id)
	}
	if c.Task != "" {
		args = append(args, "--", c.Task)
	}
	return args
}
func (s *server) observeNativeTurn() (string, string, error) {
	s.mu.Lock()
	kind := s.config.Harness
	s.mu.Unlock()
	adapter, err := adapterFor(kind)
	if err != nil {
		return "", "", err
	}
	return adapter.observe(s)
}

func (s *server) observeCodexTurn() (string, string, error) {
	s.mu.Lock()
	c := s.config
	id := s.threadID
	baseline := s.taskBaseline
	s.mu.Unlock()
	return observeTaskTurn(nativeHome, id, c.InstructionDigest, c.RolloutMaxBytes, c.Task, baseline)
}
func (s *server) writeInput(a *nativepty.Attachment, data []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != harness.NativeActive {
		return 0, errors.New("native terminal input is gated")
	}
	n, err := a.Write(data)
	if n > 0 {
		s.inputGeneration++
	}
	return n, err
}
