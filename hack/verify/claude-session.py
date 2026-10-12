#!/usr/bin/env python3
"""Empirical stock Claude Code state probe; no production adapter or credentials.

Run inside the pinned clients image with --network none and fresh /proof tmpfs.
Raw requests, transcripts and terminal output stay in the private evidence dir.
"""
import argparse
import base64
from collections import Counter
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import uuid
from datetime import datetime, timedelta, timezone

PROMPTS = ["Remember the fixture fact silver-tern-681.",
           "Use Bash to print TOOL_FACT=ochre-mole-392, then remember that fact.",
           "Continue using both facts from this conversation."]
ANSWERS = ["CLAUDE_FIRST_COMPLETE", "CLAUDE_TOOL_COMPLETE", "CLAUDE_RESUMED_COMPLETE"]
INSTRUCTIONS = "Only this deterministic local fixture is approved. INITIAL_CLAUDE_111."
REQUESTS = []
LOCK = threading.Lock()
TOOL_RETURNED = threading.Event()
RELEASE = threading.Event()
OUT = None
EVIDENCE = {}


def persist():
    with LOCK:
        (OUT / "evidence.json").write_text(json.dumps(EVIDENCE, indent=2) + "\n")
        (OUT / "requests.private.json").write_text(json.dumps(REQUESTS, indent=2) + "\n")


def event(writer, kind, **fields):
    writer.wfile.write(("event: " + kind + "\ndata: " +
                       json.dumps({"type": kind, **fields}) + "\n\n").encode())
    writer.wfile.flush()


class Provider(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if self.path.split("?")[0].endswith("/messages/count_tokens"):
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.end_headers()
            self.wfile.write(b'{"input_tokens":32}')
            return
        encoded = json.dumps(body)
        messages = body.get("messages", [])
        ordinary = [m for m in messages if m.get("role") == "user" and
                    not any(c.get("type") == "tool_result" for c in m.get("content", [])
                            if isinstance(c, dict))]
        last_user = json.dumps(ordinary[-1]) if ordinary else ""
        turn = next((i for i, prompt in enumerate(PROMPTS) if prompt in last_user), None)
        results = [c for m in messages for c in m.get("content", [])
                   if isinstance(c, dict) and c.get("type") == "tool_result"]
        required = [] if turn is None else [PROMPTS[turn], INSTRUCTIONS]
        if turn is not None and turn >= 1:
            required += [PROMPTS[0], ANSWERS[0]]
        if turn == 2:
            required += [PROMPTS[1], ANSWERS[1], "TOOL_FACT=ochre-mole-392"]
        absent = [v for v in required if v not in encoded]
        with LOCK:
            request_id = "msg_claude_fixture_" + str(len(REQUESTS) + 1)
            REQUESTS.append({"responseId": request_id, "path": self.path, "body": body, "turn": turn,
                             "missing": absent, "toolResults": results,
                             "placeholderAuth": self.headers.get("x-api-key") == "claude-fixture-placeholder"})
        persist()
        if turn is None or absent:
            self.send_error(400, "Unrecognized turn or required history absent")
            return
        tool = turn == 1 and not results
        if turn == 1 and results:
            if "TOOL_FACT=ochre-mole-392" not in json.dumps(results):
                self.send_error(400, "Actual Bash output absent")
                return
            TOOL_RETURNED.set()
            if not RELEASE.wait(30):
                self.send_error(504, "Fixture held turn timed out")
                return
        block = ({"type": "tool_use", "id": "toolu_claude_fixture", "name": "Bash",
                  "input": {"command": "printf 'TOOL_FACT=ochre-mole-392\\n'",
                            "description": "Print deterministic fixture fact"}}
                 if tool else {"type": "text", "text": ANSWERS[turn]})
        message = {"id": request_id, "type": "message",
                   "role": "assistant", "model": body["model"], "content": [block],
                   "stop_reason": "tool_use" if tool else "end_turn", "stop_sequence": None,
                   "usage": {"input_tokens": 32, "output_tokens": 8}}
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream" if body.get("stream") else "application/json")
        self.end_headers()
        if not body.get("stream"):
            self.wfile.write(json.dumps(message).encode())
            return
        start = {**message, "content": [], "stop_reason": None,
                 "usage": {"input_tokens": 32, "output_tokens": 0}}
        event(self, "message_start", message=start)
        empty = {**block, "input": {}} if tool else {"type": "text", "text": ""}
        event(self, "content_block_start", index=0, content_block=empty)
        delta = ({"type": "input_json_delta", "partial_json": json.dumps(block["input"])}
                 if tool else {"type": "text_delta", "text": block["text"]})
        event(self, "content_block_delta", index=0, delta=delta)
        event(self, "content_block_stop", index=0)
        event(self, "message_delta", delta={"stop_reason": message["stop_reason"],
                                          "stop_sequence": None}, usage={"output_tokens": 8})
        event(self, "message_stop")


def census(native):
    found = []
    for path in sorted(native.rglob("*")):
        if path.is_file():
            data = path.read_bytes()
            row = {"path": str(path.relative_to(native)), "bytes": len(data),
                   "sha256": hashlib.sha256(data).hexdigest()}
            if path.suffix == ".jsonl":
                records = []
                for line in data.splitlines():
                    try:
                        records.append(json.loads(line))
                    except json.JSONDecodeError:
                        row["partialLines"] = row.get("partialLines", 0) + 1
                row["recordTypes"] = dict(Counter(str(r.get("type")) for r in records))
                row["recordKeys"] = sorted({k for r in records for k in r})
                row["messageTypes"] = dict(Counter(str(c.get("type")) for r in records
                    for c in r.get("message", {}).get("content", []) if isinstance(c, dict)))
                row["sessionIds"] = sorted({r["sessionId"] for r in records if r.get("sessionId")})
            found.append(row)
    return found


def launch_env(home, port):
    return {"PATH": os.environ["PATH"], "HOME": str(home), "CLAUDE_CONFIG_DIR": str(home / ".claude"),
            "TMPDIR": str(home), "XDG_CONFIG_HOME": str(home / "config"),
            "XDG_DATA_HOME": str(home / "data"), "XDG_CACHE_HOME": str(home / "cache"),
            "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1", "DISABLE_AUTOUPDATER": "1",
            "ANTHROPIC_API_KEY": "claude-fixture-placeholder",
            "ANTHROPIC_BASE_URL": "http://127.0.0.1:" + str(port),
            "CLAUDE_CODE_ENABLE_PROMPT_SUGGESTION": "false",
            "CLAUDE_CODE_DISABLE_BACKGROUND_TASKS": "1",
            "TERM": "xterm-256color", "LANG": "C.UTF-8"}


def fresh_tui_preferences(native, workspace):
    # Observed in the actual 2.1.27 launcher: VZq checks theme/onboarding,
    # qT truncates environment API keys to their final 20 characters, and
    # bypassPermissionsModeAccepted controls the isolated-sandbox acknowledgement.
    # This approves only our public placeholder and disposable workspace. It is
    # generated afresh, not transferred with the selected conversation transcript.
    preferences = {"theme": "dark", "hasCompletedOnboarding": True,
                   "lastOnboardingVersion": "2.1.27", "bypassPermissionsModeAccepted": True,
                   "customApiKeyResponses": {"approved": ["claude-fixture-placeholder"[-20:]], "rejected": []},
                   "projects": {str(workspace): {"hasTrustDialogAccepted": True}}}
    path = native / ".claude.json"
    path.write_text(json.dumps(preferences))
    path.chmod(0o600)
    return preferences


def rows(path):
    found = []
    for line in path.read_text().splitlines():
        if line.startswith("{"):
            try:
                found.append(json.loads(line))
            except json.JSONDecodeError:
                # A live journal may end in the writer's incomplete final line.
                continue
    return found


def record_hook(path):
    """Disposable probe only; same-UID hook input supplies no authority."""
    data = sys.stdin.buffer.read(65537)
    assert len(data) <= 65536, "Oversized hook input"
    payload = json.loads(data)
    assert payload.get("hook_event_name") in {"UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop"}
    row = {"observedNs": time.time_ns(), "payload": payload}
    with Path(path).open("a") as output:
        output.write(json.dumps(row) + "\n")


def probe_hooks(enabled):
    if not enabled:
        return []
    command = "python3 " + str(Path(__file__).resolve()) + " --record-hook " + str(OUT / "hooks.private.jsonl")
    settings = {"hooks": {event: [{"matcher": "", "hooks": [
        {"type": "command", "command": command, "timeout": 5}]}]
        for event in ("UserPromptSubmit", "PreToolUse", "PostToolUse", "Stop")}}
    path = OUT / "settings.private.json"
    path.write_text(json.dumps(settings))
    return ["--settings", str(path)]


def hook_rows():
    path = OUT / "hooks.private.jsonl"
    return rows(path) if path.exists() else []


class Terminal:
    """Diagnostic client of the existing production nativepty verification helper."""
    def __init__(self, helper, command, workspace, env, name):
        self.output = bytearray()
        self.probes = []
        self.raw = (OUT / (name + ".pty.private.raw")).open("wb")
        self.errors = (OUT / (name + ".helper.private.log")).open("wb")
        self.helper = subprocess.Popen([str(helper)], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                       stderr=self.errors, text=True, bufsize=1)
        launch = {"Executable": shutil.which(command[0]), "Args": command[1:],
                  "Dir": str(workspace), "Env": [k + "=" + v for k, v in env.items()],
                  "Deadline": (datetime.now(timezone.utc) + timedelta(minutes=3)).isoformat(),
                  "Size": {"Rows": 40, "Columns": 140}, "ReplayBytes": 65536}
        self.pid = self.rpc("start", launch=launch)["pid"]
        assert not self.rpc("attach").get("error")

    def rpc(self, op, **fields):
        self.helper.stdin.write(json.dumps({"op": op, **fields}) + "\n")
        self.helper.stdin.flush()
        line = self.helper.stdout.readline()
        assert line, "Native PTY helper closed at " + op
        return json.loads(line)

    def write(self, data):
        return self.rpc("write", data=base64.b64encode(data).decode())

    def read(self):
        result = self.rpc("read")
        assert not result.get("error"), result
        data = base64.b64decode(result.get("data", ""))
        self.raw.write(data)
        self.raw.flush()
        self.output.extend(data)
        for query, response in [(b"\x1b[6n", b"\x1b[1;1R"), (b"\x1b[c", b"\x1b[?1;2c"),
                                (b"\x1b[?u", b"\x1b[?0u")]:
            if query in data:
                self.write(response)
                self.probes.append(query.hex())
        return result

    def until(self, predicate, timeout=25):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            self.read()
            if predicate():
                return
            time.sleep(0.03)
        raise AssertionError("Native TUI condition timed out; inspect private PTY/journal")

    def submit(self, prompt):
        assert not self.write(b"\x1b[200~" + prompt.encode() + b"\x1b[201~").get("error")
        # A separate native Enter event avoids the automatic paste detector.
        time.sleep(0.3)
        assert not self.write(b"\r").get("error")

    def close(self):
        try:
            self.rpc("stop")
        finally:
            self.helper.stdin.close()
            self.helper.wait(10)
            self.raw.close()
            self.errors.close()


def transcript(native, thread_id=None):
    candidates = list((native / "projects").glob("*/" + (thread_id or "*") + ".jsonl"))
    return candidates[0] if len(candidates) == 1 else None


def retain_transcripts(native, label):
    """Retain bounded primary transcripts and debug logs from this fresh fixture home."""
    saved = []
    remaining = 16 << 20
    for path in sorted(list((native / "projects").glob("*/*.jsonl")) +
                       list((native / "debug").glob("*.txt"))):
        if path.is_symlink() or not path.is_file():
            continue
        relative = path.relative_to(native)
        size = path.stat().st_size
        if size > (8 << 20) or size > remaining:
            saved.append({"path": str(relative), "bytes": size, "skipped": "retention limit"})
            continue
        with path.open("rb") as source:
            data = source.read(min(8 << 20, remaining) + 1)
        if len(data) > min(8 << 20, remaining):
            saved.append({"path": str(relative), "skipped": "grew past retention limit"})
            continue
        target = OUT / (label + ".native.private") / relative
        target.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
        target.write_bytes(data)
        target.chmod(0o600)
        remaining -= len(data)
        saved.append({"path": str(relative), "artifact": str(target.relative_to(OUT)),
                      "bytes": len(data), "sha256": hashlib.sha256(data).hexdigest()})
    EVIDENCE.setdefault("retainedNative", {})[label] = saved
    persist()


def observed_answer(native, answer, thread_id=None):
    path = transcript(native, thread_id)
    diagnostic = {"requestedId": thread_id, "expectedAnswer": answer,
                  "candidatePaths": [str(p.relative_to(native))
                      for p in (native / "projects").glob("*/" + (thread_id or "*") + ".jsonl")]}
    if not path:
        EVIDENCE["lastJournalDiagnostic"] = diagnostic
        return False
    records = rows(path)
    diagnostic["recordTypes"] = dict(Counter(str(row.get("type")) for row in records))
    diagnostic["assistantBlocks"] = [{"uuid": row.get("uuid"), "stopReason": row.get("message", {}).get("stop_reason"),
        "type": block.get("type"), "textBytes": len(block.get("text", "").encode()),
        "textDigest": hashlib.sha256(block.get("text", "").encode()).hexdigest(),
        "exactMatch": block.get("text") == answer}
        for row in records if row.get("type") == "assistant"
        for block in row.get("message", {}).get("content", []) if isinstance(block, dict)][-6:]
    EVIDENCE["lastJournalDiagnostic"] = diagnostic
    # Empirical evidence only. This is not a production completed-turn observer.
    return any(row.get("type") == "assistant" and any(
        block.get("type") == "text" and block.get("text") == answer
        for block in row.get("message", {}).get("content", []) if isinstance(block, dict))
        for row in records)


def run_tui(args, base, port):
    assert args.pty_helper, "--mode tui requires --pty-helper (existing Go verification helper)"
    assert args.hooks, "TUI shutdown requires actual native lifecycle hook observation"
    EVIDENCE["ptyHelperDigest"] = hashlib.sha256(args.pty_helper.read_bytes()).hexdigest()
    EVIDENCE["completionEvidence"] = "Diagnostic assistant-journal observation; production boundary unestablished"
    thread_id = None
    source = None
    for index in range(2):
        location = base / ("tui-" + str(index))
        home, workspace = location / "home", location / "workspace"
        native = home / ".claude"
        native.mkdir(parents=True)
        workspace.mkdir()
        env = launch_env(home, port)
        EVIDENCE.setdefault("freshTuiPreferences", []).append(fresh_tui_preferences(native, workspace))
        persist()
        if source:
            target = native / "projects" / re.sub(r"[^a-zA-Z0-9]", "-", str(workspace)) / source.name
            target.parent.mkdir(parents=True)
            shutil.copyfile(source, target)
        command = [args.claude, "--allowedTools", "Bash", "--model", "claude-sonnet-4-5",
                   "--append-system-prompt", INSTRUCTIONS, "--strict-mcp-config", "--mcp-config", '{"mcpServers":{}}',
                   "--permission-mode", "bypassPermissions"]
        command += probe_hooks(args.hooks)
        if thread_id:
            command += ["--resume", thread_id, PROMPTS[2]]
        else:
            command += [PROMPTS[0]]
        terminal = Terminal(args.pty_helper, command, workspace, env, "tui-" + str(index))
        try:
            terminal.until(lambda: observed_answer(native, ANSWERS[2 if index else 0], thread_id))
            if args.hooks:
                expected_stops = 3 if index else 1
                terminal.until(lambda: sum(r["payload"].get("hook_event_name") == "Stop"
                    for r in hook_rows()) == expected_stops)
            selected = transcript(native, thread_id)
            assert selected
            if not thread_id:
                thread_id = selected.stem
                assert str(uuid.UUID(thread_id)) == thread_id
                assert not terminal.rpc("detach").get("error")
                alive = terminal.rpc("status")
                assert alive.get("pid") == terminal.pid and not alive.get("exited"), alive
                assert not terminal.rpc("attach", cursor=0).get("error")
                EVIDENCE["tuiReattachedSamePID"] = terminal.pid
                TOOL_RETURNED.clear()
                RELEASE.clear()
                terminal.submit(PROMPTS[1])
                terminal.until(TOOL_RETURNED.is_set)
                EVIDENCE["tuiHeldAfterTool"] = {"status": terminal.rpc("status"), "files": census(native)}
                retain_transcripts(native, "tui-" + str(index) + "-held-after-tool")
                if args.hooks:
                    observed = hook_rows()
                    EVIDENCE["tuiHeldHooks"] = observed
                    assert sum(r["payload"].get("hook_event_name") == "Stop" for r in observed) == 1, observed
                assert not terminal.rpc("gate").get("error")
                rejection = terminal.write(b"UNAPPROVED_NEXT_TURN\r")
                assert "gated" in rejection.get("error", ""), rejection
                EVIDENCE["tuiRejectedInput"] = rejection
                persist()
                RELEASE.set()
                terminal.until(lambda: observed_answer(native, ANSWERS[1], thread_id))
                if args.hooks:
                    terminal.until(lambda: sum(r["payload"].get("hook_event_name") == "Stop"
                        for r in hook_rows()) == 2)
            else:
                assert not terminal.rpc("gate").get("error")
            # Actual Stop observation precedes controlled process-group shutdown.
            # This helper cannot prove a Docker/Kubernetes whole-container fence.
            result = terminal.rpc("stop" if args.shutdown == "controlled" else "quit")
            EVIDENCE["turns"].append({"mode": "tui", "index": index, "sessionId": thread_id,
                                     "shutdownMode": args.shutdown, "shutdown": result,
                                     "files": census(native), "probes": terminal.probes})
            if args.hooks:
                EVIDENCE["hooks"] = hook_rows()
            persist()
            expected_reason = "forced-stop" if args.shutdown == "controlled" else "native-exit"
            assert not result.get("error") and result["exit"]["Reason"] == expected_reason, result
            assert result.get("exited") and terminal.rpc("status").get("exited"), result
            if args.shutdown == "quit":
                assert result["exit"]["Code"] == 0, result
            source = selected
            shutil.copyfile(source, OUT / ("tui-" + str(index) + ".transcript.private.jsonl"))
        finally:
            RELEASE.set()
            try:
                retain_transcripts(native, "tui-" + str(index) + "-before-stop")
            finally:
                try:
                    terminal.close()
                finally:
                    retain_transcripts(native, "tui-" + str(index) + "-after-stop")
    EVIDENCE.update(status="passed", sessionId=thread_id)


def main():
    global OUT
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--claude", default="claude")
    parser.add_argument("--output-directory", type=Path, required=True)
    parser.add_argument("--temporary-root", type=Path, default=Path("/proof"))
    parser.add_argument("--mode", choices=("headless", "tui"), default="headless")
    parser.add_argument("--pty-helper", type=Path,
                        help="existing Linux hack/verify/native-pty binary; required for tui")
    parser.add_argument("--hooks", action="store_true", help="probe fresh pinned native lifecycle hooks")
    parser.add_argument("--shutdown", choices=("controlled", "quit"), default="controlled",
                        help="TUI process shutdown after observed Stop; quit remains a diagnostic")
    args = parser.parse_args()
    OUT = args.output_directory.resolve()
    OUT.mkdir(parents=True, exist_ok=True, mode=0o700)
    args.temporary_root.mkdir(parents=True, exist_ok=True)
    base = Path(tempfile.mkdtemp(prefix="claude-state-", dir=args.temporary_root)).resolve()
    os.chmod(base, 0o700)
    server = ThreadingHTTPServer(("127.0.0.1", 0), Provider)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    EVIDENCE.update({"status": "running", "mode": args.mode, "temporaryRoot": str(base), "turns": [],
                     "fixtureDigest": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
                     "limitations": ["Experimental transcript-only copy; not a production checkpoint adapter",
                         "Headless completion only; native TUI boundary and quit remain unassessed",
                         "No Docker-to-Kubernetes mobility, gateway authority or containment claim",
                         "Original approved instructions held invariant; changed instructions unassessed"]})
    persist()
    try:
        if args.mode == "tui":
            version_home = base / "version-home"
            version_home.mkdir()
            version = subprocess.check_output([args.claude, "--version"],
                env=launch_env(version_home, server.server_port), text=True).strip()
            EVIDENCE["nativeVersion"] = version
            assert version == "2.1.27 (Claude Code)", version
            run_tui(args, base, server.server_port)
            return
        thread_id = None
        source = None
        for index in range(3):
            location = base / str(index)
            home, workspace = location / "home", location / "workspace"
            native = home / ".claude"
            native.mkdir(parents=True)
            workspace.mkdir()
            env = launch_env(home, server.server_port)
            if index == 0:
                version = subprocess.check_output([args.claude, "--version"], env=env, text=True, timeout=15).strip()
                EVIDENCE["nativeVersion"] = version
                assert version == "2.1.27 (Claude Code)", version
                help_result = subprocess.check_output([args.claude, "--help"], env=env, text=True, timeout=15)
                (OUT / "help.private.txt").write_text(help_result)
            if source:
                # This is an explicit hypothesis to test, not a trusted storage adapter.
                project = re.sub(r"[^a-zA-Z0-9]", "-", str(workspace))
                target = native / "projects" / project / source.name
                target.parent.mkdir(parents=True)
                shutil.copyfile(source, target)
                EVIDENCE.setdefault("restores", []).append({"source": str(source), "target": str(target),
                    "sameBytes": source.read_bytes() == target.read_bytes(), "filesBeforeLaunch": census(native)})
            command = [args.claude, "--print", "--verbose", "--output-format", "stream-json",
                       "--allowedTools", "Bash",
                       "--model", "claude-sonnet-4-5", "--append-system-prompt", INSTRUCTIONS,
                       "--strict-mcp-config", "--mcp-config", '{"mcpServers":{}}',
                       "--permission-mode", "bypassPermissions"]
            command += probe_hooks(args.hooks)
            if thread_id:
                command += ["--resume", thread_id]
            command += [PROMPTS[index]]
            stdout, stderr = OUT / (str(index) + ".stdout.private.jsonl"), OUT / (str(index) + ".stderr.private.log")
            with stdout.open("w") as out, stderr.open("w") as err:
                process = subprocess.Popen(command, cwd=workspace, env=env, stdout=out, stderr=err)
                try:
                    if index == 1:
                        assert TOOL_RETURNED.wait(25), "Actual tool result did not reach fixture"
                        EVIDENCE["heldAfterTool"] = {"processAlive": process.poll() is None,
                                                     "files": census(native)}
                        if args.hooks:
                            observed = hook_rows()
                            EVIDENCE["heldHooks"] = observed
                            assert sum(r["payload"].get("hook_event_name") == "Stop"
                                       for r in observed) == 1, observed
                        persist()
                        RELEASE.set()
                    code = process.wait(45)
                finally:
                    RELEASE.set()
                    if process.poll() is None:
                        process.kill()
                        process.wait(5)
            events = rows(stdout)
            ids = sorted({row["session_id"] for row in events if row.get("session_id")})
            completed = [r for r in events if r.get("type") == "result"]
            EVIDENCE["turns"].append({"index": index, "exitCode": code, "sessionIds": ids,
                "resultEvents": completed, "files": census(native), "workspace": str(workspace)})
            persist()
            assert code == 0 and len(ids) == 1, "Native launch failed; inspect private stderr/events"
            if thread_id is None:
                thread_id = ids[0]
                assert str(uuid.UUID(thread_id)) == thread_id
            assert ids == [thread_id], "Native resume changed UUID"
            assert len(completed) == 1 and completed[0].get("result") == ANSWERS[index], completed
            assert completed[0].get("is_error") is False, completed
            if args.hooks:
                observed = hook_rows()
                EVIDENCE["hooks"] = observed
                assert sum(r["payload"].get("hook_event_name") == "Stop"
                           for r in observed) == index + 1, observed
            candidates = list((native / "projects").glob("*/" + thread_id + ".jsonl"))
            assert len(candidates) == 1, "Exact native transcript is absent or ambiguous"
            source = candidates[0]
            shutil.copyfile(source, OUT / (str(index) + ".transcript.private.jsonl"))
        EVIDENCE.update(status="passed", sessionId=thread_id)
    except Exception as error:
        EVIDENCE.update(status="failed", failure=str(error))
        raise
    finally:
        persist()
        server.shutdown()
        summary = {k: EVIDENCE[k] for k in ("status", "nativeVersion", "sessionId", "failure") if k in EVIDENCE}
        print(json.dumps(summary))


if __name__ == "__main__":
    if len(sys.argv) == 3 and sys.argv[1] == "--record-hook":
        record_hook(sys.argv[2])
    else:
        main()
