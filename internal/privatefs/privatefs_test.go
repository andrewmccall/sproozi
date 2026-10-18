package privatefs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testDirectory(t *testing.T) string {
	t.Helper()
	path, err := TempDir("", "sproozi-privatefs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(path); err != nil {
			t.Error(err)
		}
	})
	return path
}

func TestPrivateDirectoryAndDurableReplacement(t *testing.T) {
	dir := filepath.Join(testDirectory(t), "nested", "state")
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := EnsureDir(dir); err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		want := fmt.Sprintf(`{"sequence":%d}`, i)
		if err := Replace(dir, "record.json", []byte(want)); err != nil {
			t.Fatal(err)
		}
		got, err := ReadFile(filepath.Join(dir, "record.json"), 128)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("reopened record = %q, want %q", got, want)
		}
		var record struct {
			Sequence int `json:"sequence"`
		}
		if err = json.Unmarshal(got, &record); err != nil || record.Sequence != i {
			t.Fatalf("decode %q: %v", got, err)
		}
		if err = Check(filepath.Join(dir, "record.json"), false); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "record.json" {
		t.Fatalf("unexpected entries: %v", entries)
	}
	if _, err = ReadFile(filepath.Join(dir, "record.json"), 2); err == nil {
		t.Fatal("unbounded read accepted")
	}
}

func TestReplacementRejectsInvalidDestination(t *testing.T) {
	dir := testDirectory(t)
	for _, leaf := range []string{"", ".", "..", "../escape", `..\escape`, "name:stream", "trailing.", "trailing ", "nul\x00"} {
		if err := Replace(dir, leaf, []byte("secret")); err == nil {
			t.Fatalf("accepted destination %q", leaf)
		}
	}
	target := filepath.Join(dir, "directory")
	if err := EnsureDir(target); err != nil {
		t.Fatal(err)
	}
	if err := Replace(dir, "directory", []byte("secret")); err == nil {
		t.Fatal("replaced directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("failed write left temporary data: %v", entries)
	}
}

func TestLockProcessExclusionAndRelease(t *testing.T) {
	dir := testDirectory(t)
	path := filepath.Join(dir, "owner.lock")
	first, err := Lock(path)
	if err != nil {
		t.Fatal(err)
	}
	if output, err := lockChild(t, path, "try").CombinedOutput(); err != nil || string(output) != "blocked\n" {
		t.Fatalf("contender: %q %v", output, err)
	}
	if err = UnlockClose(first); err != nil {
		t.Fatal(err)
	}
	if output, err := lockChild(t, path, "try").CombinedOutput(); err != nil || string(output) != "acquired\n" {
		t.Fatalf("reacquire: %q %v", output, err)
	}
	child := lockChild(t, path, "hold")
	output, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	if ready, err := bufio.NewReader(output).ReadString('\n'); err != nil || ready != "acquired\n" {
		t.Fatalf("child ready: %q %v", ready, err)
	}
	if other, err := Lock(path); err == nil {
		_ = UnlockClose(other)
		t.Fatal("acquired child-owned lock")
	}
	if err = child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	recovered, err := Lock(path)
	if err != nil {
		t.Fatalf("lock persisted after owner process exit: %v", err)
	}
	if err = UnlockClose(recovered); err != nil {
		t.Fatal(err)
	}
}

func lockChild(t *testing.T, path, mode string) *exec.Cmd {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestLockProcessHelper$")
	cmd.Env = append(os.Environ(), "SPROOZI_PRIVATEFS_TEST_LOCK="+path, "SPROOZI_PRIVATEFS_TEST_MODE="+mode)
	return cmd
}

func TestLockProcessHelper(t *testing.T) {
	path := os.Getenv("SPROOZI_PRIVATEFS_TEST_LOCK")
	if path == "" {
		return
	}
	f, err := Lock(path)
	if err != nil {
		fmt.Println("blocked")
		os.Exit(0)
	}
	fmt.Println("acquired")
	if os.Getenv("SPROOZI_PRIVATEFS_TEST_MODE") == "hold" {
		time.Sleep(time.Minute)
	}
	_ = UnlockClose(f)
	os.Exit(0)
}

func TestPrivateReadAndLockRejectSymlinks(t *testing.T) {
	dir := testDirectory(t)
	if err := Replace(dir, "target", []byte("secret")); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "target"), link); err != nil {
		t.Skipf("symbolic link creation unavailable: %v", err)
	}
	if err := Check(link, false); err == nil {
		t.Fatal("accepted symbolic link")
	}
	if _, err := ReadFile(link, 64); err == nil {
		t.Fatal("read symbolic link")
	}
	if lock, err := Lock(link); err == nil {
		_ = UnlockClose(lock)
		t.Fatal("locked symbolic link")
	}
	if err := Replace(dir, "link", []byte("changed")); err == nil {
		t.Fatal("replaced symbolic link")
	}
	got, err := ReadFile(filepath.Join(dir, "target"), 64)
	if err != nil || strings.TrimSpace(string(got)) != "secret" {
		t.Fatalf("target changed: %q %v", got, err)
	}
}

func TestReadersObserveCompleteReplacements(t *testing.T) {
	dir := testDirectory(t)
	if err := Replace(dir, "journal", []byte(`{"sequence":0}`)); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		for i := range 50 {
			if err := Replace(dir, "journal", fmt.Appendf(nil, `{"sequence":%d}`, i)); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	for range 50 {
		data, err := ReadFile(filepath.Join(dir, "journal"), 128)
		if err != nil {
			t.Fatal(err)
		}
		var record struct {
			Sequence int `json:"sequence"`
		}
		if err = json.Unmarshal(data, &record); err != nil {
			t.Fatalf("incomplete record %q: %v", data, err)
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
