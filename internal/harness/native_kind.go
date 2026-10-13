package harness

import "errors"

// NativeKind selects an explicit, evidence-pinned interactive client. The zero
// value is invalid: an incoming launch never silently selects another harness.
type NativeKind string

const (
	NativeCodex  NativeKind = "codex"
	NativeClaude NativeKind = "claude-code"
)

func (k NativeKind) Validate() error {
	_, err := NativeVersion(k)
	return err
}

func NativeVersion(k NativeKind) (string, error) {
	switch k {
	case NativeCodex:
		return CodexSessionVersion, nil
	case NativeClaude:
		return ClaudeSessionVersion, nil
	default:
		return "", errors.New("unsupported native harness")
	}
}

// NativeDirectory is the selected component of the atomic workspace/native
// checkpoint pair, independent of fresh cache, credentials and launch settings.
func NativeDirectory(k NativeKind) (string, error) {
	switch k {
	case NativeCodex:
		return "codex", nil
	case NativeClaude:
		return "claude", nil
	default:
		return "", errors.New("unsupported native harness")
	}
}
