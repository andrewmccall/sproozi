#!/usr/bin/env python3
"""Capture the Claude transcript and hook fixtures from a stock client.

Runs one scripted turn of the given Claude Code binary in its interactive
terminal, launched as cmd/native-session launches a managed session: the same
fixed tools, strict MCP configuration, sproozi and profile plugins, hook
events, setting sources, permission mode and onboarding preferences. A local
provider scripts the model, a local stdio MCP server stands in for the
gateway's connections, and the turn ends as the supervisor ends it: the
process group is killed after Stop. Nothing is typed into the terminal, so the
client must submit the task and complete the turn unattended in a fresh home.
No network, credentials or host configuration reach the client.

The transcript and hook payloads are then scrubbed (see README.md) and written
to internal/harness/testdata and cmd/native-session/testdata. With --check,
the run only proves the unattended turn and writes nothing.

usage: capture.py --claude PATH [--check] SCENARIO [SCENARIO ...]
"""
import argparse
import json
import os
from pathlib import Path
import pty
import re
import select
import shutil
import signal
import subprocess
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = Path(__file__).resolve().parents[3]
ANSWER = "FINAL_ANSWER"
MODEL = "claude-sonnet-4-5-20250929"
BASH_WAIT = {"command": "sleep 3; echo slept", "description": "Wait for MCP"}
BASH_FAIL = {"command": "sleep 3; echo slept; exit 3", "description": "Wait for MCP"}
FIXTURE_SKILL = "fixture-skill"

# Each scenario: the task, the tool calls the scripted model makes in order,
# and whether the fixture MCP tool reports an error. The first Bash call also
# gives the MCP server time to connect, as Claude Code connects it after the
# first request.
SCENARIOS = {
    "mcp-tool": ("--literal task: call the tool\n  then report.  ",
                 [("Bash", BASH_WAIT), ("mcp__fixture__verify", {})], False),
    "failed-tools": ("--literal task: call the tool",
                     [("Bash", BASH_FAIL), ("mcp__fixture__verify", {})], True),
    "profile-skill": ("Use the fixture skill.",
                      [("Skill", {"skill": "profile:" + FIXTURE_SKILL})], False),
    "sproozi-skill": ("Use the sproozi access skill.",
                      [("Skill", {"skill": "sproozi:sproozi-access"})], False),
}

MCP_SERVER = r'''
import json, os, sys
for line in sys.stdin:
    m = json.loads(line)
    if "id" not in m:
        continue
    method = m.get("method")
    result = {}
    if method == "initialize":
        result = {"protocolVersion": m["params"].get("protocolVersion", "2024-11-05"),
                  "capabilities": {"tools": {}}, "serverInfo": {"name": "fixture", "version": "1"}}
    elif method == "tools/list":
        result = {"tools": [{"name": "verify", "description": "Return the fixture marker.",
                             "inputSchema": {"type": "object", "properties": {}}}]}
    elif method == "tools/call":
        if os.environ.get("FIXTURE_MCP_ERROR"):
            result = {"content": [{"type": "text", "text": "FIXTURE_FAILED"}], "isError": True}
        else:
            result = {"content": [{"type": "text", "text": "FIXTURE_MARKER"}]}
    sys.stdout.write(json.dumps({"jsonrpc": "2.0", "id": m["id"], "result": result}) + "\n")
    sys.stdout.flush()
'''

HOOK = '#!/bin/sh\ncat >> "$(dirname "$0")/hooks.jsonl"\necho >> "$(dirname "$0")/hooks.jsonl"\n'

FIXTURE_SKILL_MD = ("---\nname: " + FIXTURE_SKILL + "\ndescription: \"Use when the task asks for the "
                    "fixture skill.\"\n---\n\nReply with FINAL_ANSWER.\n")


class Provider(BaseHTTPRequestHandler):
    calls = []

    def log_message(self, *_):
        pass

    def do_GET(self):
        self.reply("application/json", b"{}")

    def reply(self, kind, body):
        self.send_response(200)
        self.send_header("Content-Type", kind)
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        request = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))) or b"{}")
        if not request.get("stream"):
            self.reply("application/json", json.dumps({
                "id": "msg_aux", "type": "message", "role": "assistant", "model": request.get("model"),
                "content": [{"type": "text", "text": "DONE_TEXT"}], "stop_reason": "end_turn",
                "stop_sequence": None, "usage": {"input_tokens": 10, "output_tokens": 5}}).encode())
            return
        offered = [t.get("name") for t in request.get("tools", [])]
        made = sum(1 for m in request.get("messages", []) if isinstance(m.get("content"), list)
                   for b in m["content"] if b.get("type") == "tool_use")
        call = self.calls[made] if made < len(self.calls) else None
        if call and call[0] not in offered:
            call = None
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        message = "msg_%d" % time.time_ns()

        def event(name, data):
            self.wfile.write(("event: %s\ndata: %s\n\n" % (name, json.dumps(data))).encode())
        event("message_start", {"type": "message_start", "message": {
            "id": message, "type": "message", "role": "assistant", "content": [], "model": request.get("model"),
            "stop_reason": None, "stop_sequence": None, "usage": {"input_tokens": 10, "output_tokens": 0}}})
        if call:
            event("content_block_start", {"type": "content_block_start", "index": 0, "content_block": {
                "type": "tool_use", "id": "toolu_%d" % time.time_ns(), "name": call[0], "input": {}}})
            event("content_block_delta", {"type": "content_block_delta", "index": 0, "delta": {
                "type": "input_json_delta", "partial_json": json.dumps(call[1])}})
            stop = "tool_use"
        else:
            event("content_block_start", {"type": "content_block_start", "index": 0,
                                          "content_block": {"type": "text", "text": ""}})
            event("content_block_delta", {"type": "content_block_delta", "index": 0,
                                          "delta": {"type": "text_delta", "text": ANSWER}})
            stop = "end_turn"
        event("content_block_stop", {"type": "content_block_stop", "index": 0})
        event("message_delta", {"type": "message_delta", "delta": {"stop_reason": stop, "stop_sequence": None},
                                "usage": {"output_tokens": 5}})
        event("message_stop", {"type": "message_stop"})


def write(path, text, mode=0o600):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text)
    path.chmod(mode)


def managed(*args):
    """A file the supervisor installs, rendered by the same Go code."""
    return subprocess.check_output(["go", "run", "./hack/verify/claude-transcripts/managed", *args],
                                   cwd=ROOT, text=True)


def layout(root):
    """Write the managed launch's files as cmd/native-session does."""
    skill = managed("skill")
    write(root / "sproozi-plugin/.claude-plugin/plugin.json",
          json.dumps({"description": "Sproozi integration", "name": "sproozi"}), 0o400)
    write(root / "sproozi-plugin/skills/sproozi-access/SKILL.md", skill, 0o400)
    write(root / "profile-plugin/.claude-plugin/plugin.json",
          json.dumps({"description": "Skills from your Sproozi profile", "name": "profile"}), 0o400)
    write(root / "profile-plugin/skills" / FIXTURE_SKILL / "SKILL.md", FIXTURE_SKILL_MD, 0o400)
    write(root / "observe-claude", HOOK, 0o700)
    write(root / "mcp-server.py", MCP_SERVER)
    write(root / "settings.json", managed("settings", str(root / "observe-claude")))
    workspace = root / "ws"
    workspace.mkdir()
    write(root / "home/.claude/.claude.json", json.dumps({
        "theme": "dark", "hasCompletedOnboarding": True, "bypassPermissionsModeAccepted": True,
        "projects": {str(workspace): {"hasTrustDialogAccepted": True}},
        "customApiKeyResponses": {"approved": ["sproozi-local-placeholder"[-20:]], "rejected": []}}))


def screen(output):
    """The terminal's text, without control sequences, for a failure report."""
    text = re.sub(rb"\x1b\[[0-9;?<>=]*[A-Za-z]|\x1b[\]P][^\x07\x1b]*(\x07|\x1b\\)", b"", output)
    return text.decode("utf-8", "replace")[-1500:]


def drive(binary, root, args, env, task):
    """Run the client in a terminal until Stop, then kill its process group.

    Nothing is typed: the managed launch must submit its task and complete
    the turn unattended, in a fresh home, as a remote task does.
    """
    pid, fd = pty.fork()
    if pid == 0:
        os.chdir(root / "ws")
        os.execve(binary, [binary] + args, env)
    output, deadline, stopped = b"", time.time() + 60, False
    while time.time() < deadline:
        ready, _, _ = select.select([fd], [], [], 0.5)
        if ready:
            try:
                data = os.read(fd, 65536)
            except OSError:
                break
            output += data
            if b"\x1b[6n" in data:
                os.write(fd, b"\x1b[1;1R")
        hooks = root / "hooks.jsonl"
        if hooks.exists() and '"hook_event_name":"Stop"' in hooks.read_text():
            stopped = True
            time.sleep(2)
            break
    try:
        os.killpg(pid, signal.SIGKILL)
    except OSError:
        pass
    # A killed process can wait on unread terminal output; closing the
    # terminal releases it.
    os.close(fd)
    os.waitpid(pid, 0)
    hooks = root / "hooks.jsonl"
    prompts = [json.loads(line).get("prompt") for line in (hooks.read_text() if hooks.exists() else "").splitlines()
               if '"hook_event_name":"UserPromptSubmit"' in line]
    if prompts != [task]:
        sys.exit("the task was not submitted as the turn's prompt; the terminal shows:\n" + screen(output))
    if not stopped:
        sys.exit("the task's turn never reported Stop; the terminal shows:\n" + screen(output))


def scrub(root, line):
    """Replace capture paths with the managed ones and elide opaque context."""
    workspace = str(root / "ws")
    for old, new in [(re.sub(r"[^a-zA-Z0-9]", "-", workspace), "-workspace-session-workspace"),
                     (workspace, "/workspace/session/workspace"),
                     (str(root / "home/.claude"), "/workspace/session/claude"),
                     (str(root / "sproozi-plugin"), "/tmp/sproozi-native/sproozi-plugin"),
                     (str(root / "profile-plugin"), "/tmp/sproozi-native/profile-plugin"),
                     (str(root / "observe-claude"), "/usr/local/bin/native-session observe-claude")]:
        line = line.replace(old, new)
    row = json.loads(line)
    kind = row.get("type")
    if kind == "attachment" and "attachment" in row:
        # Environment, system prompt and skill listings name the host.
        row["attachment"] = {"type": row["attachment"]["type"]}
        row.pop("rendered", None)
    elif kind == "api-request-shape":
        row = {k: row[k] for k in ("type", "sessionId", "shapeHash", "timestamp", "version") if k in row}
    elif kind == "api-request":
        row = {k: row[k] for k in ("type", "sessionId", "id", "timestamp", "version", "shapeHash") if k in row}
    elif kind == "api-request-blob":
        row["message"]["content"] = [{"type": "text", "text": "elided"}]
    for key in ("gitBranch", "slug"):
        row.pop(key, None)
    text = json.dumps(row, separators=(",", ":"), ensure_ascii=False)
    for private in (str(root), os.environ.get("HOME", "/nonexistent"), os.environ.get("USER", "\0")):
        if private and private in text:
            sys.exit("scrubbed fixture still names a host path: " + text[:200])
    return text


def capture(binary, scenario, version, check):
    task, calls, mcp_error = SCENARIOS[scenario]
    Provider.calls = calls
    server = ThreadingHTTPServer(("127.0.0.1", 0), Provider)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    root = Path(tempfile.mkdtemp(prefix="claude-capture-")).resolve()
    try:
        layout(root)
        mcp = {"mcpServers": {"fixture": {"type": "stdio", "command": sys.executable,
                                          "args": [str(root / "mcp-server.py")],
                                          "env": {"FIXTURE_MCP_ERROR": "1" if mcp_error else ""}}}}
        write(root / "mcp.json", json.dumps(mcp))
        env = {"PATH": os.environ["PATH"], "HOME": str(root / "home"), "TMPDIR": str(root),
               "TERM": "xterm-256color", "CLAUDE_CONFIG_DIR": str(root / "home/.claude"),
               "ANTHROPIC_API_KEY": "sproozi-local-placeholder",
               "ANTHROPIC_BASE_URL": "http://127.0.0.1:%d" % server.server_port,
               "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "DISABLE_AUTOUPDATER": "1",
               "CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION": "false", "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS": "1"}
        tools = "Bash,Skill"
        args = ["--tools", tools, "--allowedTools", tools, "--strict-mcp-config", "--mcp-config", str(root / "mcp.json"),
                "--plugin-dir", str(root / "sproozi-plugin"), "--plugin-dir", str(root / "profile-plugin"),
                "--setting-sources", "", "--settings", str(root / "settings.json"),
                "--permission-mode", "bypassPermissions", "--append-system-prompt", "Fixture instructions.",
                "--model", MODEL, "--", task]
        drive(binary, root, args, env, task)
        transcripts = list((root / "home/.claude/projects").glob("*/*.jsonl"))
        if len(transcripts) != 1:
            sys.exit("expected one transcript, found %d" % len(transcripts))
        name = "claude-%s-%s" % (version, scenario)
        rows = [scrub(root, line) for line in transcripts[0].read_text().splitlines() if line]
        hooks = [scrub(root, line) for line in (root / "hooks.jsonl").read_text().splitlines() if line.strip()]
        if check:
            print("%s: the task was submitted unattended and its turn completed" % name)
            return
        (ROOT / "internal/harness/testdata" / (name + ".jsonl")).write_text("\n".join(rows) + "\n")
        (ROOT / "cmd/native-session/testdata" / (name + "-hooks.jsonl")).write_text("\n".join(hooks) + "\n")
        print("%s: session %s, %d transcript rows, %d hooks" % (name, transcripts[0].stem, len(rows), len(hooks)))
    finally:
        server.shutdown()
        shutil.rmtree(root, ignore_errors=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--claude", required=True, help="stock Claude Code binary")
    parser.add_argument("--check", action="store_true", help="prove the unattended turn; write no fixture")
    parser.add_argument("scenario", nargs="+", choices=sorted(SCENARIOS))
    options = parser.parse_args()
    version = subprocess.check_output([options.claude, "--version"], text=True).split()[0]
    for scenario in options.scenario:
        capture(options.claude, scenario, version, options.check)


if __name__ == "__main__":
    main()
