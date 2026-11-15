package harness

// NativeKind selects an explicit interactive client. The zero
// value is invalid: an incoming launch never silently selects another harness.
type NativeKind string

const (
	NativeCodex   NativeKind = "codex"
	NativeClaude  NativeKind = "claude-code"
	NativeCopilot NativeKind = "copilot"
)

// Validate accepts only the registry's managed kinds.
func (k NativeKind) Validate() error {
	_, err := Managed(k)
	return err
}

// TakesTask reports a managed kind whose supervisor submits a task as the
// launch's prompt and binds completion to that prompt's turn: Codex by the
// task text in its rollout, Claude Code by its UserPromptSubmit hook.
func (k NativeKind) TakesTask() bool {
	return k == NativeCodex || k == NativeClaude
}

// NativeDirectory is the selected component of the atomic workspace/native
// checkpoint pair, independent of fresh cache, credentials and launch settings.
func NativeDirectory(k NativeKind) (string, error) {
	h, err := Managed(k)
	if err != nil {
		return "", err
	}
	return h.NativeDirectory, nil
}
