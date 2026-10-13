package harness_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
)

const (
	nativeFailureCase = "native-failure"
)

const successCase = "success"

// fakeOpenCode is an external command fixture. It deliberately returns zero
// for incomplete/error sessions, reproducing the stock client's exit behavior.
const fakeOpenCode = `
import json, os, signal, sys, time
from pathlib import Path
state = Path(os.environ["FIXTURE_STATE"])
case = os.environ["FIXTURE_CASE"]
args = sys.argv[1:]
if "run" in args:
    state.touch()
    if case == "cancel":
        signal.signal(signal.SIGTERM, lambda *_: (state.with_suffix(".stopped").touch(), sys.exit(143)))
        state.with_suffix(".ready").touch()
        time.sleep(30)
    sys.exit(7 if case == "native-failure" else 0)
elif "list" in args:
    if not state.exists() and case != "existing":
        sys.exit(0) # Stock session list emits no bytes for an empty database.
    print(json.dumps([{"id":"root"}] * (2 if case == "ambiguous" else 1)))
elif "export" in args:
    if case == "malformed":
        print("broken JSON")
        sys.exit(0)
    info = {"role":"assistant", "parentID":"user", "finish":"stop", "time":{"completed":1}}
    parts = [{"type":"step-finish", "reason":"stop"}, {"type":"text", "text":"fixture result"}]
    if case == "error": info["error"] = {}
    if case == "incomplete": info["time"] = {}
    if case == "wrong-parent": info["parentID"] = "other"
    if case == "wrong-finish": parts[0]["reason"] = "length"
    print(json.dumps({"messages":[{"info":{"role":"user", "id":"user"}}, {"info":info, "parts":parts}]}))
`

func launchFixture(t *testing.T, scenario string) (*exec.Cmd, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal("OpenCode launch verification requires Python 3", err)
	}
	directory := t.TempDir()
	for name, data := range map[string]string{
		"opencode":  "#!" + python + "\n" + fakeOpenCode,
		"launch.py": harness.OpenCodeLaunch,
	} {
		if err := os.WriteFile(filepath.Join(directory, name), []byte(data), 0700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(ctx, python, filepath.Join(directory, "launch.py"), "--model", "fixture-model")
	state := filepath.Join(directory, "state")
	cmd.Env = append(os.Environ(), "PATH="+directory+":"+os.Getenv("PATH"), "FIXTURE_CASE="+scenario, "FIXTURE_STATE="+state)
	return cmd, state
}

func TestOpenCodeLaunchReportsNativeCompletion(t *testing.T) {
	for _, scenario := range []string{successCase, "error", "incomplete", "wrong-parent", "wrong-finish", "existing", "ambiguous", "malformed", nativeFailureCase} {
		t.Run(scenario, func(t *testing.T) {
			cmd, _ := launchFixture(t, scenario)
			output, err := cmd.CombinedOutput()
			want := 1
			switch scenario {
			case "success":
				want = 0
			case nativeFailureCase:
				want = 7
			}
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != want {
				t.Fatalf("want exit %d: %v, %s", want, err, output)
			}
			completed := strings.Contains(string(output), `"result": "fixture result"`)
			if completed != (scenario == successCase) {
				t.Fatalf("incorrect completion output: %s", output)
			}
		})
	}
}

func TestOpenCodeLaunchForwardsTermination(t *testing.T) {
	cmd, state := launchFixture(t, "cancel")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, state+".ready")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if cmd.ProcessState.ExitCode() != 143 {
		t.Fatalf("termination exit = %d", cmd.ProcessState.ExitCode())
	}
	waitForFile(t, state+".stopped")
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("native command did not start")
}
