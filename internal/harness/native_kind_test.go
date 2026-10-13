package harness

import "testing"

func TestNativeKindHasNoImplicitSelection(t *testing.T) {
	for _, tc := range []struct {
		kind               NativeKind
		version, directory string
	}{
		{NativeCodex, "0.161.0", "codex"}, {NativeClaude, "2.1.27", "claude"},
	} {
		version, err := NativeVersion(tc.kind)
		if err != nil || version != tc.version {
			t.Fatalf("native version: %q %v", version, err)
		}
		directory, err := NativeDirectory(tc.kind)
		if err != nil || directory != tc.directory {
			t.Fatalf("native directory: %q %v", directory, err)
		}
	}
	for _, kind := range []NativeKind{"", "unknown"} {
		if kind.Validate() == nil {
			t.Fatal("implicit or unknown native selection accepted")
		}
	}
}
