package session

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Production code judges deadlines against the wall clock alone: only tests
// open a coordinator with another clock.
func TestOnlyTestsReplaceTheCoordinatorClock(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "bin" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
			path == filepath.Join(root, "internal", "session", "store.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "WithClock(") {
			t.Errorf("%s replaces the coordinator's clock", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
