package workspace

import (
	"strings"
	"testing"
)

const testSelectedIgnoredFile = "needed.bin"

func TestIgnoredSelectionBoundary(t *testing.T) {
	t.Parallel()
	for _, paths := range [][]string{nil, {}, {testSelectedIgnoredFile, "cache/chosen"}} {
		if err := ValidateIgnoredPaths(paths); err != nil {
			t.Fatalf("valid file selection %v: %v", paths, err)
		}
	}
	for _, paths := range [][]string{{""}, {"../outside"}, {"/outside"}, {"a/../outside"}, {"a//b"}, {"a\\b"}, {"a\x00b"}, {"a\nb"}, {".git/config"}, {"cache/.GIT/hooks"}, {".gitmodules"}, {testSelectedIgnoredFile, testSelectedIgnoredFile}, {strings.Repeat("a", 513)}, make([]string, 33)} {
		if err := ValidateIgnoredPaths(paths); err == nil {
			t.Fatalf("unsafe or excessive file selection accepted: %q", paths)
		}
	}
}
