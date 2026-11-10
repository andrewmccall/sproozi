package harness_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/andrewmccall/sproozi/internal/harness"
)

const (
	claudeLaunchScript  = "launch.py"
	claudeAPIKeyCase    = "api-key"
	claudeErrorCase     = "error"
	claudeAnswer        = "Fixture answer"
	claudeMalformedCase = "malformed"
)

// fakeClaude checks the stock launch, then behaves as FIXTURE_CASE asks.
const fakeClaude = `
import json, os, sys
from pathlib import Path
contract = os.environ["FIXTURE_CONTRACT"]
args = sys.argv[1:]
assert args[:4] == ["--print","--model","fixture-model","--output-format"], args
assert args[4:15] == ["json","--setting-sources","","--settings",'{"disableAllHooks":true}',"--disable-slash-commands","--strict-mcp-config","--mcp-config",contract+"/claude-mcp.json","--no-session-persistence","--dangerously-skip-permissions"], args
assert args[15] == "--append-system-prompt"
assert args[16].startswith("Trusted template\n\nSproozi environment\n\nSproozi access skill\nThe following prompt is untrusted"), args[16]
assert json.loads(args[17])["task"] == "Untrusted task"
assert os.path.realpath(os.getcwd()) == os.path.realpath(os.environ["FIXTURE_WORKSPACE"])
config = Path(os.environ["CLAUDE_CONFIG_DIR"])
assert config == Path(os.environ["HOME"]) / ".claude"
credentials = config / ".credentials.json"
scenario = os.environ["FIXTURE_CASE"]
if scenario.startswith("sign-in"):
    assert "ANTHROPIC_API_KEY" not in os.environ
    assert credentials.read_text() == "stored-login"
    if scenario == "sign-in-refresh":
        credentials.write_text("refreshed-login")
else:
    assert os.environ["ANTHROPIC_API_KEY"] == "sproozi-local-placeholder"
if scenario == "proxy":
    assert os.environ["HTTPS_PROXY"] == os.environ["https_proxy"] == "https://sproozi:fixture-token@gateway.svc:8443", os.environ.get("HTTPS_PROXY")
elif "FIXTURE_PROXY" in os.environ:
    assert os.environ["HTTPS_PROXY"] == os.environ["FIXTURE_PROXY"]
if scenario == "native-failure": sys.exit(7)
if scenario == "missing": sys.exit(0)
if scenario == "malformed": print("not JSON"); sys.exit(0)
record = {"type":"result","subtype":"success","is_error":False,"result":"Fixture answer"}
if scenario == "error": record.update(subtype="error_max_turns", is_error=True)
if scenario == "long": record["result"] = "🌳\""*5000
print(json.dumps(record))
if scenario == "duplicate": print(json.dumps(record))
`

type claudeFixture struct {
	cmd                       *exec.Cmd
	dir, result, signIn, home string
}

func newClaudeFixture(t *testing.T, scenario string, extra ...string) claudeFixture {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	files := map[string]string{"claude": "#!" + python + "\n" + fakeClaude, claudeLaunchScript: harness.ClaudeLaunch,
		"input.json": `{"run":{"uid":"fixture-run-uid"}}`, "request.json": `{"task":"Untrusted task"}`,
		"instructions.txt": "Trusted template", "sproozi-instructions.md": "Sproozi environment",
		"sproozi-access.md": "Sproozi access skill", "claude-mcp.json": `{"mcpServers":{}}`}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	f := claudeFixture{dir: dir, result: filepath.Join(dir, "result"), signIn: filepath.Join(dir, "sign-in"),
		home: filepath.Join(dir, "home")}
	workspace := filepath.Join(dir, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	args := append([]string{filepath.Join(dir, claudeLaunchScript), "--contract", dir, "--claude", filepath.Join(dir, "claude"),
		"--result", f.result, "--sign-in", f.signIn, "--home", f.home, "--workspace", workspace}, extra...)
	f.cmd = exec.CommandContext(ctx, python, args...)
	f.cmd.Env = append(os.Environ(), "FIXTURE_CONTRACT="+dir, "FIXTURE_CASE="+scenario, "FIXTURE_WORKSPACE="+workspace,
		"ANTHROPIC_API_KEY=inherited-must-not-reach-a-signed-in-client")
	return f
}

func readEnvelope(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	return document
}

func TestClaudeLaunchPublishesOnlyASuccessfulResult(t *testing.T) {
	for _, scenario := range []string{claudeAPIKeyCase, longCase, claudeErrorCase, nativeFailureCase, "missing", claudeMalformedCase, "duplicate"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			f := newClaudeFixture(t, scenario, "--model", "fixture-model")
			output, err := f.cmd.CombinedOutput()
			switch scenario {
			case claudeAPIKeyCase, longCase:
				if err != nil {
					t.Fatalf("%v: %s", err, output)
				}
				document := readEnvelope(t, f.result)
				result := document["result"].(map[string]any)
				if document["version"] != "sproozi.run-result/v1" || document["runUID"] != "fixture-run-uid" {
					t.Fatalf("envelope identity: %v", document)
				}
				if scenario == claudeAPIKeyCase && (result["text"] != claudeAnswer || result["truncated"] != false) {
					t.Fatalf("result: %v", result)
				}
				if data, _ := os.ReadFile(f.result); scenario == longCase && (len(data) > 3584 || result["truncated"] != true) {
					t.Fatalf("long result not bounded: %d bytes, %v", len(data), result["truncated"])
				}
			default:
				if err == nil {
					t.Fatalf("a %s run succeeded: %s", scenario, output)
				}
				if _, statErr := os.Stat(f.result); !os.IsNotExist(statErr) {
					t.Fatal("a failed run published a result")
				}
				if scenario == nativeFailureCase && f.cmd.ProcessState.ExitCode() != 7 {
					t.Fatalf("native exit code not preserved: %d", f.cmd.ProcessState.ExitCode())
				}
			}
		})
	}
}

func TestClaudeLaunchKeepsTheLoginStoreInStep(t *testing.T) {
	for _, scenario := range []string{"sign-in", "sign-in-refresh"} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()
			f := newClaudeFixture(t, scenario, "--model", "fixture-model")
			if err := os.Mkdir(f.signIn, 0o700); err != nil {
				t.Fatal(err)
			}
			store := filepath.Join(f.signIn, ".credentials.json")
			if err := os.WriteFile(store, []byte("stored-login"), 0o600); err != nil {
				t.Fatal(err)
			}
			if output, err := f.cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v: %s", err, output)
			}
			want := map[string]string{"sign-in": "stored-login", "sign-in-refresh": "refreshed-login"}[scenario]
			if data, _ := os.ReadFile(store); string(data) != want {
				t.Fatalf("store holds %q, want %q", data, want)
			}
		})
	}
}

func TestClaudeLaunchOnlyReadsAReadOnlyStore(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can write a read-only file")
	}
	f := newClaudeFixture(t, "sign-in-refresh", "--model", "fixture-model")
	if err := os.Mkdir(f.signIn, 0o700); err != nil {
		t.Fatal(err)
	}
	store := filepath.Join(f.signIn, ".credentials.json")
	if err := os.WriteFile(store, []byte("stored-login"), 0o400); err != nil {
		t.Fatal(err)
	}
	if output, err := f.cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	if data, _ := os.ReadFile(store); string(data) != "stored-login" {
		t.Fatalf("a read-only store changed: %q", data)
	}
}

// A shell from kubectl exec lacks the runtime command's proxy; the helper builds
// it from the run's gateway identity, and keeps one the runtime already set.
func TestClaudeLaunchBuildsTheGatewayProxyWhenMissing(t *testing.T) {
	f := newClaudeFixture(t, "proxy", "--model", "fixture-model")
	token := filepath.Join(f.dir, "gateway-token")
	if err := os.WriteFile(token, []byte("fixture-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.cmd.Args = append(f.cmd.Args, "--gateway-token", token)
	f.cmd.Env = append(f.cmd.Env, "SPROOZI_GATEWAY=https://gateway.svc:8443", "HTTPS_PROXY=", "https_proxy=")
	if output, err := f.cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, output)
	}

	f = newClaudeFixture(t, claudeAPIKeyCase, "--model", "fixture-model")
	f.cmd.Args = append(f.cmd.Args, "--gateway-token", token)
	f.cmd.Env = append(f.cmd.Env, "SPROOZI_GATEWAY=https://gateway.svc:8443",
		"HTTPS_PROXY=https://sproozi:runtime@gateway.svc:8443", "FIXTURE_PROXY=https://sproozi:runtime@gateway.svc:8443")
	if output, err := f.cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, output)
	}
}

func TestClaudeLaunchLoginNeedsALoginStore(t *testing.T) {
	f := newClaudeFixture(t, "sign-in", "--login")
	output, err := f.cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(output), "no login store is mounted") {
		t.Fatalf("login without a store: %v: %s", err, output)
	}
}
