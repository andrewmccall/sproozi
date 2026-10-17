package main

import (
	"encoding/json"
	"errors"

	"github.com/andrewmccall/sproozi/internal/harness"
	"github.com/andrewmccall/sproozi/internal/nativepty"
)

func nativePaths(kind harness.NativeKind) (string, string, error) {
	switch kind {
	case harness.NativeCodex:
		return nativeHome, executable, nil
	case harness.NativeClaude:
		return claudeHome, claudeExecutable, nil
	default:
		return "", "", errors.New("unsupported native harness")
	}
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
	c := s.config
	id := s.threadID
	baseline := s.taskBaseline
	s.mu.Unlock()
	if c.Harness == harness.NativeClaude {
		return s.observeClaudeTurn()
	}
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
