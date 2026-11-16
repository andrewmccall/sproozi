package harness_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
)

// signedInFixture is a Claude Code launch whose login store holds
// storedLogin.
func signedInFixture(t *testing.T, scenario string, extra ...string) (claudeFixture, string) {
	t.Helper()
	f := newClaudeFixture(t, scenario, append([]string{"--model", "fixture-model"}, extra...)...)
	if err := os.Mkdir(f.signIn, 0o700); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(f.signIn, ".credentials.json")
	if err := os.WriteFile(store, []byte(storedLogin), 0o600); err != nil {
		t.Fatal(err)
	}
	return f, store
}

func storeHolds(t *testing.T, store, want string) {
	t.Helper()
	if data, _ := os.ReadFile(store); string(data) != want {
		t.Fatalf("store holds %q, want %q", data, want)
	}
}

// Once Claude Code is killed, its login file is not saved, even as a whole
// login: the helper records the kill before it forwards a signal, and when a
// signal ended Claude Code.
func TestClaudeLaunchSavesNothingAfterAKill(t *testing.T) {
	t.Run("forwarded signal", func(t *testing.T) {
		t.Parallel()
		f, store := signedInFixture(t, "sign-in-killed")
		if err := f.cmd.Start(); err != nil {
			t.Fatal(err)
		}
		ready := filepath.Join(f.dir, "ready")
		for until := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			if time.Now().After(until) {
				t.Fatal("the fixture never wrote its login")
			}
		}
		if err := f.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if err := f.cmd.Wait(); err == nil {
			t.Fatal("a terminated run succeeded")
		}
		storeHolds(t, store, storedLogin)
	})
	t.Run("ended by a signal", func(t *testing.T) {
		t.Parallel()
		f, store := signedInFixture(t, "sign-in-signalled")
		if output, err := f.cmd.CombinedOutput(); err == nil {
			t.Fatalf("a signalled run succeeded: %s", output)
		}
		storeHolds(t, store, storedLogin)
	})
}

// A store another holder keeps locked fails the launch within the helper's
// bound rather than holding it for ever.
func TestClaudeLaunchDoesNotWaitForALockedStore(t *testing.T) {
	f, store := signedInFixture(t, "sign-in", "--sign-in-wait", "0.2")
	peer, err := os.OpenFile(store, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Close() }()
	if err = syscall.Flock(int(peer.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	output, err := f.cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "locked") || time.Since(started) > 5*time.Second {
		t.Fatalf("a locked store: %v after %s: %s", err, time.Since(started), output)
	}
}

// The helper's store write, stopped after any number of operations, leaves
// the old login, the new one, or a file that is not a whole login: never an
// empty store. A write whose save's deadline has passed writes nothing.
func TestClaudeLaunchStoreWriteNeverStopsInAStateAReaderActsOn(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "launch.py")
	if err = os.WriteFile(script, []byte(harness.ClaudeLaunch), 0o600); err != nil {
		t.Fatal(err)
	}
	const check = `
import importlib.util, json, sys, time
spec = importlib.util.spec_from_file_location("launch", sys.argv[1])
launch = importlib.util.module_from_spec(spec)
spec.loader.exec_module(launch)

class Stalled(Exception):
    pass

def attempt(old, new, budget):
    content = bytearray(old)
    left = [budget]
    def spend():
        if left[0] <= 0:
            raise Stalled()
        left[0] -= 1
    def pwrite(_, data, offset):
        for i, b in enumerate(data):
            spend()
            while len(content) <= offset + i:
                content.append(0)
            content[offset + i] = b
    def ftruncate(_, size):
        spend()
        del content[size:]
    def fsync(_):
        spend()
    try:
        launch.write_store(0, new, time.monotonic() + 60, pwrite, ftruncate, fsync)
        return bytes(content), True
    except Stalled:
        return bytes(content), False

for old, new in ((b'{"t":"aaaa"}', b'{"t":"bbbbbbbb"}'), (b'{"t":"bbbbbbbb"}', b'{"t":"aaaa"}'), (b"", b'{"t":"a"}')):
    budget = 0
    while True:
        got, finished = attempt(old, new, budget)
        acted = launch.non_empty(got) is None or launch.whole_login(got)
        assert not (acted and got not in (old, new)), (old, new, budget, got)
        assert not (launch.non_empty(got) is None and old), (old, new, budget, got)
        if finished:
            assert got == new, got
            break
        budget += 1

try:
    launch.write_store(0, b'{"t":"a"}', time.monotonic() - 1,
                       lambda *a: sys.exit("wrote"), lambda *a: sys.exit("wrote"), lambda *a: sys.exit("wrote"))
    sys.exit("a write past its deadline went ahead")
except ValueError:
    pass
`
	if output, err := exec.Command(python, "-I", "-c", check, script).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
}
