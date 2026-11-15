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

// TakesTask reports the kind's registry TakesTask: a managed kind whose
// supervisor submits a task as the launch's prompt and binds completion to
// that prompt's turn.
func (k NativeKind) TakesTask() bool {
	h, err := Managed(k)
	return err == nil && h.TakesTask
}

// NativeSessionRoot is the managed session's directory in its sandbox: the
// workspace and every harness's native home are under it.
const NativeSessionRoot = "/workspace/session"

// NativeHomePath is the kind's native home in the sandbox. It is empty for a
// kind that is not managed.
func NativeHomePath(k NativeKind) string {
	h, err := Managed(k)
	if err != nil {
		return ""
	}
	return NativeSessionRoot + "/" + h.NativeDirectory
}

// NativeExecutable is the stock client's path in the managed sandbox image,
// empty for a kind that is not managed.
func NativeExecutable(k NativeKind) string {
	h, err := Managed(k)
	if err != nil {
		return ""
	}
	return h.Executable
}

// NativeDisplayName names the kind in messages, such as "Claude Code".
func NativeDisplayName(k NativeKind) string {
	h, err := Managed(k)
	if err != nil {
		return string(k)
	}
	return h.DisplayName
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
