package harness

import "testing"

func TestNativeKindHasNoImplicitSelection(t *testing.T) {
	for kind, expected := range map[NativeKind]string{NativeCodex: "codex", NativeClaude: "claude"} {
		if err := kind.Validate(); err != nil {
			t.Fatal(err)
		}
		directory, err := NativeDirectory(kind)
		if err != nil || directory != expected {
			t.Fatalf("native directory: %q %v", directory, err)
		}
	}
	for _, kind := range []NativeKind{"", "unknown"} {
		if kind.Validate() == nil {
			t.Fatal("implicit or unknown native selection accepted")
		}
	}
}
