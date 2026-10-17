package harness

import "errors"

// NativeKind selects an explicit interactive client. The zero
// value is invalid: an incoming launch never silently selects another harness.
type NativeKind string

const (
	NativeCodex  NativeKind = "codex"
	NativeClaude NativeKind = "claude-code"
)

func (k NativeKind) Validate() error {
	switch k {
	case NativeCodex, NativeClaude:
		return nil
	default:
		return errors.New("unsupported native harness")
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
