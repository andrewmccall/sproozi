package session

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/testutil"
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

// Tests that pass a deadline replace the coordinator's clock; one opened
// without options reads the machine's clock at the moment it judges a
// deadline.
func TestOpenJudgesDeadlinesOnTheMachineClock(t *testing.T) {
	t.Parallel()
	c, err := Open(filepath.Join(testutil.PrivateDir(t), "journal"),
		map[string]Placement{testLocal: newPlacement(nil, "")}, testMaxCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	before := time.Now()
	now := c.now()
	after := time.Now()
	if now.Before(before) || now.After(after) {
		t.Fatalf("coordinator clock read %s, want between %s and %s", now, before, after)
	}
}
