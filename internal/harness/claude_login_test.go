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

// withoutPeriodicSyncs runs the fixture's helper with no sync between the
// one before launch and the save at exit, as the supervisor's kill tests do
// (quietInterval in cmd/native-session). A periodic sync rightly saves a
// refresh it reads while Claude Code runs, so with one due every second the
// store after a kill would depend on whether the machine reached the kill
// before the next sync.
func withoutPeriodicSyncs(f claudeFixture) {
	const run = `
import importlib.util, sys
spec = importlib.util.spec_from_file_location("launch", sys.argv[1])
launch = importlib.util.module_from_spec(spec)
spec.loader.exec_module(launch)
assert isinstance(launch.LOGIN_INTERVAL, float)
launch.LOGIN_INTERVAL = 24 * 3600.0
sys.argv = sys.argv[1:]
sys.exit(launch.main())
`
	f.cmd.Args = append([]string{f.cmd.Args[0], "-c", run}, f.cmd.Args[1:]...)
}

// terminateWhenReady starts the fixture, waits for it to write its ready
// file and sends the helper SIGTERM, as Kubernetes does to a Pod's PID 1.
func terminateWhenReady(t *testing.T, f claudeFixture) {
	t.Helper()
	if err := f.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	ready := filepath.Join(f.dir, "ready")
	for until := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("the fixture never became ready")
		}
	}
	if err := f.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := f.cmd.Wait(); err == nil {
		t.Fatal("a terminated run succeeded")
	}
}

// Once Claude Code is killed, its login file is not saved, even as a whole
// login: the helper records the kill before it forwards a signal, and when a
// signal ended Claude Code. A refresh made before the kill reaches the store
// only through a periodic sync that read it while Claude Code ran.
func TestClaudeLaunchSavesNothingAfterAKill(t *testing.T) {
	t.Run("forwarded signal", func(t *testing.T) {
		t.Parallel()
		f, store := signedInFixture(t, "sign-in-killed")
		withoutPeriodicSyncs(f)
		terminateWhenReady(t, f)
		storeHolds(t, store, storedLogin)
	})
	t.Run("ended by a signal", func(t *testing.T) {
		t.Parallel()
		f, store := signedInFixture(t, "sign-in-signalled")
		withoutPeriodicSyncs(f)
		if output, err := f.cmd.CombinedOutput(); err == nil {
			t.Fatalf("a signalled run succeeded: %s", output)
		}
		storeHolds(t, store, storedLogin)
	})
	// Periodic syncs run: any that reads the login Claude Code writes as the
	// forwarded signal reaches it already sees the kill.
	t.Run("login written as the signal arrives", func(t *testing.T) {
		t.Parallel()
		f, store := signedInFixture(t, "sign-in-refreshed-as-killed")
		terminateWhenReady(t, f)
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

def attempt(old, new, budget, short):
    """Write new over old; after budget units every operation fails. With
    short, only bytes spend the budget, as on a filling disk: a write past it
    returns a short count instead of raising, as os.pwrite does, and truncates
    and syncs go on succeeding."""
    content = bytearray(old)
    left = [budget]
    def spend():
        if left[0] <= 0:
            raise Stalled()
        left[0] -= 1
    def pwrite(_, data, offset):
        for i, b in enumerate(data):
            if short and left[0] <= 0:
                return i
            spend()
            while len(content) <= offset + i:
                content.append(0)
            content[offset + i] = b
        return len(data)
    def ftruncate(_, size):
        if not short:
            spend()
        del content[size:]
    def fsync(_):
        if not short:
            spend()
    try:
        launch.write_store(0, new, time.monotonic() + 60, pwrite, ftruncate, fsync)
        return bytes(content), True
    except (Stalled, OSError):
        return bytes(content), False

for old, new in ((b'{"t":"aaaa"}', b'{"t":"bbbbbbbb"}'), (b'{"t":"bbbbbbbb"}', b'{"t":"aaaa"}'), (b"", b'{"t":"a"}'),
                 (b'{"refreshToken":"AAAAAAAA"}', b'{"refreshToken":"BBBBBBBB"}')):
    for short in (False, True):
        budget = 0
        while True:
            got, finished = attempt(old, new, budget, short)
            acted = launch.non_empty(got) is None or launch.whole_login(got)
            assert not (acted and got not in (old, new)), (old, new, budget, short, got)
            assert not (launch.non_empty(got) is None and old), (old, new, budget, short, got)
            if finished:
                assert got == new, (short, got)
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

// The helper's stop waits at most its bound, even for a periodic sync stuck
// in open or read on a stalled volume, and reads a store os.read would
// return in pieces whole.
func TestClaudeLaunchStopIsBoundedAndReadsTheWholeStore(t *testing.T) {
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
import importlib.util, os, sys, threading, time
spec = importlib.util.spec_from_file_location("launch", sys.argv[1])
launch = importlib.util.module_from_spec(spec)
spec.loader.exec_module(launch)
directory = sys.argv[2]

sync = launch.LoginSync(directory + "/store", directory + "/native", 0.2)
sync.thread = threading.Thread(target=lambda: time.sleep(30), daemon=True)  # stuck in a read
sync.thread.start()
started = time.monotonic()
sync.stop()
assert time.monotonic() - started < 2, time.monotonic() - started

# An ordinary stop, uncontended and with a change to save, takes about a
# millisecond: it never waits for its five-second bound.
with open(directory + "/ordinary-store", "w") as handle:
    handle.write('{"claudeAiOauth":{"refreshToken":"stored"}}')
sync = launch.LoginSync(directory + "/ordinary-store", directory + "/ordinary-native")
sync.start()
with open(directory + "/ordinary-native", "w") as handle:
    handle.write('{"claudeAiOauth":{"refreshToken":"refreshed"}}')
started = time.monotonic()
sync.stop()
assert time.monotonic() - started < 2, time.monotonic() - started
with open(directory + "/ordinary-store") as handle:
    assert "refreshed" in handle.read()

store = directory + "/whole"
with open(store, "wb") as handle:
    handle.write(b'{"claudeAiOauth":{"refreshToken":"' + b"x" * 200000 + b'"}}')
pread = os.pread
os.pread = lambda fd, n, offset: pread(fd, min(n, 4096), offset)
descriptor = os.open(store, os.O_RDONLY)
assert launch.whole_login(launch.read_all(descriptor))
`
	if output, err := exec.Command(python, "-I", "-c", check, script, dir).CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
}
