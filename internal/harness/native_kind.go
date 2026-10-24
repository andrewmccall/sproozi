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

// NativeDirectory is the selected component of the atomic workspace/native
// checkpoint pair, independent of fresh cache, credentials and launch settings.
func NativeDirectory(k NativeKind) (string, error) {
	h, err := Managed(k)
	if err != nil {
		return "", err
	}
	return h.NativeDirectory, nil
}
