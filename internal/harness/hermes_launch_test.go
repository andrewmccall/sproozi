package harness_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
)

const (
	longCase = "long"
)

const fakeHermes = `
import json, os, signal, subprocess, sys, time
from pathlib import Path
assert sys.argv[1:] == ["chat","--oneshot","--query-file",os.environ["FIXTURE_CONTRACT"]+"/request.json","--format","stream-json","--provider","openai-api","--model","fixture-model","--max-turns","16","--ignore-rules","--toolsets",os.environ.get("FIXTURE_TOOLSETS","sproozi-mcp-docs")]
assert os.environ["HERMES_EPHEMERAL_SYSTEM_PROMPT"] == "Trusted template"
assert json.loads(Path(os.environ["FIXTURE_CONTRACT"]+"/request.json").read_text())["task"] == "Untrusted task"
assert os.environ["OPENAI_API_KEY"] == "sproozi-local-placeholder"
config=json.loads(Path(os.environ["HERMES_HOME"]+"/config.yaml").read_text())
assert config["mcp_servers"]["sproozi-mcp-docs"]["url"] == "https://gateway.svc/mcp/docs"
if os.environ.get("FIXTURE_TOOLSETS"):
 assert config["mcp_servers"]["sproozi-kubernetes"]["url"] == "https://kubernetes.default.svc/mcp"
scenario=os.environ["FIXTURE_CASE"]
state=Path(os.environ["FIXTURE_STATE"])
if scenario == "cancel":
 signal.signal(signal.SIGTERM,lambda *_:(state.with_suffix(".stopped").touch(),sys.exit(143)))
 state.with_suffix(".ready").touch()
 time.sleep(30)
if scenario == "native-failure": sys.exit(7)
if scenario == "missing": sys.exit(0)
if scenario == "malformed": print("invalid JSON");sys.exit(0)
if scenario == "duplicate-key": print('{"type":"result","exit_code":0,"text":"first","text":"other"}');sys.exit(0)
record={"type":"result","exit_code":0,"text":"Fixture answer"}
if scenario == "error": record["error"]="failed"
if scenario == "failed-result": record["exit_code"]=1
if scenario == "long": record["text"]="🌳\""*5000
if scenario == "detached-stdout":
 child = subprocess.Popen([sys.executable,"-c","import time; time.sleep(30)"],start_new_session=True)
 state.with_suffix(".child-pid").write_text(str(child.pid))
print(json.dumps(record))
if scenario == "duplicate-result": print(json.dumps(record))
`

func hermesFixture(t *testing.T, scenario string) (*exec.Cmd, string, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	files := map[string]string{"hermes": "#!" + python + "\n" + fakeHermes, "launch.py": harness.HermesLaunch, "input.json": `{"run":{"uid":"fixture-run-uid"}}`, "request.json": `{"task":"Untrusted task"}`, "instructions.txt": "Trusted template", "hermes-mcp.json": `{"mcp_servers":{"sproozi-mcp-docs":{"url":"https://gateway.svc/mcp/docs"}}}`}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	output := filepath.Join(dir, "result")
	cmd := exec.CommandContext(ctx, python, filepath.Join(dir, "launch.py"), "--model", "fixture-model", "--contract", dir, "--hermes", filepath.Join(dir, "hermes"), "--result", output)
	state := filepath.Join(dir, "state")
	cmd.Env = append(os.Environ(), "HERMES_HOME="+filepath.Join(dir, "home"), "FIXTURE_CONTRACT="+dir, "FIXTURE_CASE="+scenario, "FIXTURE_STATE="+state)
	return cmd, output, state
}

func TestHermesLaunchPublishesOnlyNativeSuccessfulResult(t *testing.T) {
	for _, scenario := range []string{successCase, longCase, "missing", "malformed", "duplicate-key", "duplicate-result", "error", "failed-result", "native-failure"} {
		t.Run(scenario, func(t *testing.T) {
			cmd, result, _ := hermesFixture(t, scenario)
			output, err := cmd.CombinedOutput()
			want := 1
			if scenario == successCase || scenario == longCase {
				want = 0
			}
			if scenario == "native-failure" {
				want = 7
			}
			if cmd.ProcessState == nil || cmd.ProcessState.ExitCode() != want {
				t.Fatalf("want exit%d: %v %s", want, err, output)
			}
			document, readErr := os.ReadFile(result)
			if want != 0 {
				if !os.IsNotExist(readErr) {
					t.Fatalf("Failed native run published a result: %s", document)
				}
				return
			}
			if readErr != nil || len(document) > 3584 {
				t.Fatalf("invalid bounded result %v bytes%d", readErr, len(document))
			}
			var got struct {
				Version string `json:"version"`
				RunUID  string `json:"runUID"`
				Result  struct {
					Text      string `json:"text"`
					Truncated bool   `json:"truncated"`
				} `json:"result"`
			}
			if err := json.Unmarshal(document, &got); err != nil {
				t.Fatal(err)
			}
			if got.Version != "sproozi.run-result/v1" || got.RunUID != "fixture-run-uid" {
				t.Fatalf("Result ownership %s", document)
			}
			if scenario == successCase && (got.Result.Text != "Fixture answer" || got.Result.Truncated) {
				t.Fatalf("result %s", document)
			}
			if scenario == longCase && (!got.Result.Truncated || got.Result.Text == "") {
				t.Fatalf("missing explicit truncation %s", document)
			}
		})
	}
}

func TestHermesLaunchForwardsNativeTermination(t *testing.T) {
	cmd, result, state := hermesFixture(t, "cancel")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitForFile(t, state+".ready")
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	if cmd.ProcessState.ExitCode() != 143 {
		t.Fatalf("termination exit%d", cmd.ProcessState.ExitCode())
	}
	waitForFile(t, state+".stopped")
	if _, err := os.Stat(result); !os.IsNotExist(err) {
		t.Fatal("Cancelled native run published success")
	}
}

func TestHermesLaunchTrustedKubernetesOptIn(t *testing.T) {
	cmd, _, _ := hermesFixture(t, successCase)
	cmd.Args = append(cmd.Args, "--kubernetes-mcp")
	cmd.Env = append(cmd.Env, "FIXTURE_TOOLSETS=sproozi-kubernetes,sproozi-mcp-docs")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("Native Kubernetes opt-in failed: %v %s", err, output)
	}
}

func TestHermesLaunchCompletesWhenDetachedChildRetainsStdout(t *testing.T) {
	cmd, result, state := hermesFixture(t, "detached-stdout")
	// A real file avoids the test's own output collector waiting for the detached
	// child's inherited stderr. Only the launcher's native stdout pipe may block.
	logFile, err := os.Create(state + ".output")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	waitForFile(t, state+".child-pid")
	pidBytes, err := os.ReadFile(state + ".child-pid")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(pidBytes))
	if err != nil || pid <= 0 {
		t.Fatalf("Invalid detached fixture child PID %q", pidBytes)
	}
	// The host-side launcher is not PID 1, so it cannot adopt this detached
	// descendant. The test owns its cleanup even if the assertion fails.
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err = <-done:
		if err != nil {
			output, _ := os.ReadFile(state + ".output")
			t.Fatalf("Completed native run failed: %v %s", err, output)
		}
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(pid, syscall.SIGKILL)
		_ = cmd.Process.Kill()
		<-done
		t.Fatal("Native completion waited for detached child stdout EOF")
	}
	document, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Result struct {
			Text string `json:"text"`
		} `json:"result"`
	}
	if err = json.Unmarshal(document, &got); err != nil || got.Result.Text != "Fixture answer" {
		t.Fatalf("Native answer was not retained: %v %s", err, document)
	}
}
