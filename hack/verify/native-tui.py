#!/usr/bin/env python3
"""Exercise stock Codex TUI through the production Go PTY and native state adapter.

Only fresh temporary homes/workspaces and a deterministic loopback provider.
No app-server remote protocol, Docker, user credentials or paid provider.
"""
import argparse
import base64
import hashlib
from datetime import datetime, timedelta, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import tempfile
import threading
import time

OUT = None
HELPER = None
REPORT = None
ROOT = Path(__file__).resolve().parents[2]
REQUESTS = []
FIRST = "Remember the TUI fixture fact: lilac-marten-683."
SECOND = "Remember another TUI fixture fact: copper-swift-927."
THIRD = "Continue using the two facts already in this conversation."
ANSWERS = ["TUI_FIRST_COMPLETE", "TUI_SECOND_COMPLETE", "TUI_RESUMED_COMPLETE"]
DEVELOPERS = ["TUI_INITIAL_APPROVED_111", "TUI_INITIAL_APPROVED_111"]
STARTED = threading.Event()
RELEASE = threading.Event()
EVIDENCE = {}
LOCK = threading.RLock()


def persist():
    with LOCK:
        (OUT / "evidence.json").write_text(json.dumps(EVIDENCE, indent=2) + "\n")
        (OUT / "requests.json").write_text(json.dumps(REQUESTS, indent=2) + "\n")


def event(writer, kind, **fields):
    writer.wfile.write(("data: " + json.dumps({"type": kind, **fields}) + "\n\n").encode())
    writer.wfile.flush()


class Provider(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        users = ["".join(c.get("text", "") for c in item.get("content", []))
                 for item in body.get("input", []) if item.get("role") == "user"]
        last_user = users[-1] if users else ""
        is_title = last_user.startswith("Generate a concise, single-line task title")
        turn_index = {FIRST: 0, SECOND: 1, THIRD: 2}.get(last_user)
        with LOCK:
            index = len(REQUESTS)
            REQUESTS.append({"path": self.path, "body": body,
                             "kind": "title" if is_title else "conversation",
                             "turn": turn_index,
                             "placeholderAuth": self.headers.get("Authorization") == "Bearer tui-placeholder"})
            persist()
        if not is_title and turn_index is None:
            self.send_error(400, "Unexpected conversation request")
            return
        encoded = json.dumps(body)
        required = [] if is_title else [[FIRST], [FIRST, ANSWERS[0], SECOND],
                    [FIRST, ANSWERS[0], SECOND, ANSWERS[1], THIRD]][turn_index]
        missing = [item for item in required if item not in encoded]
        with LOCK:
            REQUESTS[index]["missingHistory"] = missing
            REQUESTS[index]["developerMarkers"] = {
                marker: marker in encoded for marker in DEVELOPERS
            }
            persist()
        if missing:
            self.send_error(400, "Required conversation history absent")
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        response = {"id": "resp_tui_" + str(index), "object": "response",
                    "created_at": int(time.time()), "status": "in_progress",
                    "model": body["model"], "output": []}
        item = {"id": "msg_tui_" + str(index), "type": "message", "status": "in_progress",
                "role": "assistant", "content": []}
        part = {"type": "output_text", "text": "", "annotations": []}
        event(self, "response.created", response=response)
        if not is_title and turn_index == 1:
            STARTED.set()
            if not RELEASE.wait(40):
                EVIDENCE["providerReleaseTimeout"] = True
                persist()
        answer = "Remember TUI facts" if is_title else ANSWERS[turn_index]
        event(self, "response.output_item.added", output_index=0, item=item)
        event(self, "response.content_part.added", item_id=item["id"], output_index=0, content_index=0, part=part)
        event(self, "response.output_text.delta", item_id=item["id"], output_index=0, content_index=0, delta=answer)
        event(self, "response.output_text.done", item_id=item["id"], output_index=0, content_index=0, text=answer)
        part["text"] = answer
        item.update(status="completed", content=[part])
        event(self, "response.content_part.done", item_id=item["id"], output_index=0, content_index=0, part=part)
        event(self, "response.output_item.done", output_index=0, item=item)
        response.update(status="completed", output=[item], usage={"input_tokens": 10, "output_tokens": 5, "total_tokens": 15})
        event(self, "response.completed", response=response)
        with LOCK:
            REQUESTS[index]["completed"] = True
            persist()


def clean_terminal(raw):
    text = raw.decode(errors="replace")
    text = re.sub(r"\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)", "", text)
    return re.sub(r"\x1b\[[0-9;?<>:]*[ -/]*[@-~]", "", text)


class Connection:
    def __init__(self, supervisor):
        self.supervisor = supervisor

    def sendall(self, data):
        self.supervisor.command(data)

    def recv(self, _):
        return bytes(self.supervisor.data[-65536:])

    def close(self):
        self.supervisor.attached = False
        self.supervisor.rpc("detach")


class Supervisor:
    """Fixture client for the genuine production Go nativepty package."""
    def __init__(self, cmd, cwd, env, name, base):
        self.name, self.data, self.probes, self.rejected = name, bytearray(), [], []
        self.metrics = []
        self.stopped, self.attached, self.returncode = False, False, None
        self.lock = threading.RLock()
        self.transport = subprocess.Popen([str(HELPER)], stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=(OUT / (name + ".helper.stderr")).open("wb"),
            text=True, bufsize=1)
        launch = {"Executable": shutil.which(cmd[0]), "Args": cmd[1:],
                  "Dir": str(cwd), "Env": [k + "=" + v for k, v in env.items()],
                  "Deadline": (datetime.now(timezone.utc) + timedelta(minutes=3)).isoformat(),
                  "Size": {"Rows": 40, "Columns": 140}, "ReplayBytes": 65536}
        self.pid = self.rpc("start", launch=launch)["pid"]
        self.proc = self
        self.thread = threading.Thread(target=self.loop, daemon=True)
        self.thread.start()

    def rpc(self, op, **fields):
        with self.lock:
            self.transport.stdin.write(json.dumps({"op": op, **fields}) + "\n")
            self.transport.stdin.flush()
            line = self.transport.stdout.readline()
            if not line:
                raise RuntimeError("Production PTY helper closed during " + op)
            return json.loads(line)

    def loop(self):
        pending = bytearray()
        with (OUT / (self.name + ".pty.raw")).open("wb") as raw:
            while not self.stopped:
                if not self.attached:
                    time.sleep(0.03)
                    continue
                result = self.rpc("read")
                if result.get("error"):
                    if "detached" in result["error"]:
                        continue
                    self.reader_error = result["error"]
                    return
                chunk = base64.b64decode(result.get("data", ""))
                if not chunk:
                    time.sleep(0.01)
                    continue
                raw.write(chunk)
                raw.flush()
                self.data.extend(chunk)
                (OUT / (self.name + ".terminal.txt")).write_text(clean_terminal(bytes(self.data)))
                pending.extend(chunk)
                probes = [(b"\x1b[6n", b"\x1b[1;1R", "cursor"),
                          (b"\x1b[c", b"\x1b[?1;2c", "primary-device"),
                          (b"\x1b[?u", b"\x1b[?0u", "keyboard"),
                          (b"\x1b]11;?\x1b\\", b"\x1b]11;rgb:0000/0000/0000\x1b\\", "background"),
                          (b"\x1b]10;?\x1b\\", b"\x1b]10;rgb:ffff/ffff/ffff\x1b\\", "foreground")]
                for query, answer, label in probes:
                    while query in pending:
                        pos = pending.index(query)
                        del pending[pos:pos + len(query)]
                        self.command(answer)
                        self.probes.append(label)
                pending = pending[-100:]

    def connect(self):
        result = self.rpc("attach", cursor=0)
        assert not result.get("error"), result
        self.metrics.append({"receivedBytes": 0, "forwardedBytes": 0, "rejectedBytes": 0})
        self.attached = True
        return Connection(self)

    def command(self, data):
        if isinstance(data, str):
            data = data.encode()
        result = self.rpc("write", data=base64.b64encode(data).decode())
        metric = self.metrics[-1]
        metric["receivedBytes"] += len(data)
        metric["forwardedBytes"] += result.get("written", 0)
        if result.get("error"):
            metric["rejectedBytes"] += len(data)
            if "gated" in result["error"]:
                self.rejected.append(data.decode(errors="replace"))
            else:
                raise RuntimeError(result["error"])
        else:
            assert result.get("written") == len(data), result

    @property
    def gated(self):
        return self._gated

    @gated.setter
    def gated(self, value):
        assert value
        result = self.rpc("gate")
        assert not result.get("error"), result
        self._gated = True

    def native_command(self, text):
        assert text == "/quit"
        self.gated = True
        result = self.rpc("quit")
        assert not result.get("error"), result
        assert result["exit"]["Reason"] == "native-exit", result
        self.returncode = result["exit"]["Code"]

    def poll(self):
        return self.returncode if self.rpc("status").get("exited") else None

    def wait(self, _):
        result = self.rpc("wait")
        assert not result.get("error"), result
        self.returncode = result["exit"]["Code"]
        return self.returncode

    def wait_text(self, value, timeout=20):
        end = time.monotonic() + timeout
        while time.monotonic() < end:
            if value in clean_terminal(bytes(self.data)):
                return True
            if self.poll() is not None:
                return False
            time.sleep(0.1)
        return False

    def close(self):
        self.stopped = True
        self.thread.join(2)
        self.rpc("stop")
        self.transport.stdin.close()
        self.transport.wait(5)


def rollouts(native):
    found = []
    for path in sorted(native.rglob("*.jsonl")):
        if "sessions" not in path.parts:
            continue
        for line in path.read_text().splitlines():
            try:
                row = json.loads(line)
            except json.JSONDecodeError:
                continue
            found.append({"file": str(path.relative_to(native)), **row})
    return found


def completed(native, count, timeout=20):
    end = time.monotonic() + timeout
    while time.monotonic() < end:
        events = rollouts(native)
        marks = [row for row in events if row.get("type") == "event_msg" and
                 row.get("payload", {}).get("type") == "task_complete" and
                 not row.get("payload", {}).get("error") and
                 row.get("payload", {}).get("last_agent_message") in ANSWERS]
        if len(marks) >= count:
            return marks
        time.sleep(0.1)
    return []


def conversation_requests():
    with LOCK:
        return [request for request in REQUESTS if request.get("kind") == "conversation"]


def setup_location(base, index):
    location = base / str(index)
    home = location / "home"
    native = home / ".codex"
    workspace = location / "workspace"
    native.mkdir(parents=True)
    workspace.mkdir(parents=True)
    env = {"PATH": os.environ["PATH"], "HOME": str(home), "CODEX_HOME": str(native),
           "TMPDIR": str(location), "XDG_CONFIG_HOME": str(home / "config"),
           "XDG_DATA_HOME": str(home / "data"), "XDG_CACHE_HOME": str(home / "cache"),
           "XDG_STATE_HOME": str(home / "state"), "FIXTURE_MODEL_KEY": "tui-placeholder",
           "TERM": "xterm-256color", "COLORTERM": "truecolor", "LANG": "en_US.UTF-8"}
    return native, workspace, env


def submit(client, text):
    # Native bracketed-paste protocol plus a separate Enter key avoids relying
    # on the TUI's timing-dependent automatic paste detector.
    client.sendall(b"\x1b[200~" + text.encode() + b"\x1b[201~")
    time.sleep(0.3)
    client.sendall(b"\r")


def command(port, workspace, thread_id=None):
    config = {"model_provider": "tui-fixture", "model_providers.tui-fixture.name": "TUI fixture",
              "model_providers.tui-fixture.base_url": "http://127.0.0.1:" + str(port) + "/v1",
              "model_providers.tui-fixture.env_key": "FIXTURE_MODEL_KEY",
              "model_providers.tui-fixture.wire_api": "responses",
              "model_providers.tui-fixture.supports_websockets": False,
              "developer_instructions": "Only the deterministic TUI fixture is approved. " + DEVELOPERS[1 if thread_id else 0],
              "check_for_update_on_startup": False,
              "projects." + json.dumps(str(workspace)) + ".trust_level": "trusted"}
    cmd = ["codex", "--no-daemon", "--no-alt-screen", "--model", "gpt-6.1-sol",
           "--sandbox", "read-only", "--ask-for-approval", "never", "--cd", str(workspace)]
    for key, value in config.items():
        cmd += ["-c", key + "=" + json.dumps(value)]
    if thread_id:
        cmd += ["resume", thread_id, THIRD]
    else:
        cmd += [FIRST]
    return cmd


def main():
    global EVIDENCE, OUT, HELPER, REPORT
    parser = argparse.ArgumentParser()
    parser.add_argument("--helper", required=True, type=Path)
    parser.add_argument("--report", required=True, type=Path)
    args = parser.parse_args()
    HELPER, REPORT = args.helper.resolve(), args.report.resolve()
    base = Path(tempfile.mkdtemp(prefix="sproozi-production-tui-", dir="/private/tmp" if Path("/private/tmp").is_dir() else None)).resolve()
    OUT = base / "private-evidence"
    OUT.mkdir(mode=0o700)
    server = ThreadingHTTPServer(("127.0.0.1", 0), Provider)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    native, workspace, env = setup_location(base, 0)
    cmd = command(server.server_port, workspace)
    EVIDENCE = {"version": subprocess.check_output(["codex", "--version"], env=env, text=True).strip(),
                "temporaryRoot": str(base), "command": cmd, "steps": [], "passed": False,
                "limitations": ["Production Go PTY on this host; no Docker/Kubernetes containment proof",
                                "Fixture emits text only; no tool subprocess or interrupted side-effect proof"]}
    assert EVIDENCE["version"] == "codex-cli 0.161.0", EVIDENCE["version"]
    persist()
    supervisor = None
    resumed = None
    try:
        supervisor = Supervisor(cmd, workspace, env, "original", base)
        connection = supervisor.connect()
        EVIDENCE["pid"] = supervisor.proc.pid
        EVIDENCE["steps"].append("native TUI started with one PTY and production Go attachment")
        persist()
        assert supervisor.wait_text(ANSWERS[0], 30), "First TUI response not observed"
        assert completed(native, 1), "First native task_complete not persisted"
        events = rollouts(native)
        ids = [row["payload"]["id"] for row in events if row.get("type") == "session_meta"]
        assert len(set(ids)) == 1, ids
        thread_id = ids[0]
        EVIDENCE["nativeThreadId"] = thread_id
        EVIDENCE["steps"].append("first native completed turn observed and persisted")
        connection.close()
        time.sleep(0.5)
        assert supervisor.proc.poll() is None, "TUI died on client detach"
        connection = supervisor.connect()
        buffered = connection.recv(65536)
        assert ANSWERS[0].encode() in buffered, "Reconnection did not receive retained terminal output"
        EVIDENCE["steps"].append("detached then reattached to same living native TUI PID and PTY")
        persist()
        submit(connection, SECOND)
        assert STARTED.wait(15), "Second TUI turn did not reach fixture"
        supervisor.gated = True
        EVIDENCE["steps"].append("move pending gates terminal input while native second turn remains active")
        connection.sendall(b"DO_NOT_SUBMIT_PENDING_MOVE\r")
        time.sleep(0.5)
        assert supervisor.rejected, "Pending-move input was not rejected"
        assert supervisor.proc.poll() is None, "Input gate interrupted native process"
        assert len(completed(native, 2, 0.1)) == 0, "Second turn completed before fixture release"
        assert len(conversation_requests()) == 2, "Blocked turn reached provider"
        persist()
        RELEASE.set()
        assert supervisor.wait_text(ANSWERS[1], 20), "Active turn did not finish after gate"
        assert completed(native, 2), "Second native task_complete not persisted"
        assert len(conversation_requests()) == 2, "Input gate queued unintended future turn"
        EVIDENCE["steps"].append("active native turn finished despite gate; blocked input created no turn")
        EVIDENCE["sourceRolloutEvents"] = rollouts(native)
        EVIDENCE["rejectedInput"] = supervisor.rejected
        persist()
        # Trusted supervisor sends native command after verified completed turn.
        supervisor.native_command("/quit")
        supervisor.proc.wait(10)
        assert supervisor.proc.returncode == 0, "Native /quit exited unsuccessfully"
        EVIDENCE["steps"].append("native /quit exited successfully after task_complete")
        EVIDENCE["sourceExitCode"] = supervisor.proc.returncode
        persist()
        next_native, next_workspace, next_env = setup_location(base, 1)
        checkpoint = supervisor.rpc("checkpoint", home=str(native), threadId=thread_id, restoreHome=str(next_native))
        assert not checkpoint.get("error"), checkpoint
        EVIDENCE["checkpointSHA256"] = checkpoint["digest"]
        resumed_cmd = command(server.server_port, next_workspace, thread_id)
        EVIDENCE["resumeCommand"] = resumed_cmd
        resumed = Supervisor(resumed_cmd, next_workspace, next_env, "resumed", base)
        resume_connection = resumed.connect()
        assert resumed.wait_text(ANSWERS[2], 30), "Resumed native TUI response not observed"
        assert completed(next_native, 3), "Resumed native task_complete not persisted"
        resumed_ids = [row["payload"]["id"] for row in rollouts(next_native) if row.get("type") == "session_meta"]
        assert set(resumed_ids) == {thread_id}, resumed_ids
        turns = conversation_requests()
        assert [row["turn"] for row in turns] == [0, 1, 2], "Unexpected duplicate or reordered conversation"
        assert turns[2]["missingHistory"] == [], turns[2]
        assert all(row["placeholderAuth"] for row in REQUESTS), "Unexpected provider credential"
        assert len(turns) == 3, "Unexpected extra conversation request"
        EVIDENCE["resumedRolloutEvents"] = rollouts(next_native)
        assert turns[2]["developerMarkers"][DEVELOPERS[0]], "Initial trusted instructions absent"
        EVIDENCE["initialInstructionsPreserved"] = True
        EVIDENCE["providerRequestCensus"] = {"conversation": len(turns),
            "title": len(REQUESTS) - len(turns), "total": len(REQUESTS)}
        EVIDENCE["steps"].append("same-UUID native TUI resume in a distinct home/workspace consumes earlier two turns")
        resumed.native_command("/quit")
        resumed.proc.wait(10)
        assert resumed.proc.returncode == 0
        resume_connection.close()
        EVIDENCE["passed"] = True
        print("PASS native TUI attach/gate/quit/resume thread=" + thread_id, flush=True)
    except BaseException as exc:
        EVIDENCE["error"] = str(exc)
        raise
    finally:
        RELEASE.set()
        if supervisor:
            EVIDENCE["originalProbes"] = supervisor.probes
            EVIDENCE["attachmentCensus"] = supervisor.metrics
            supervisor.close()
        if resumed:
            EVIDENCE["resumedProbes"] = resumed.probes
            EVIDENCE["resumedAttachmentCensus"] = resumed.metrics
            resumed.close()
        persist()
        server.shutdown()
        sources = [*sorted((ROOT / "internal/nativepty").glob("*.go")),
                   ROOT / "internal/harness/codex_state.go", ROOT / "hack/verify/native-pty/main.go",
                   Path(__file__).resolve()]
        report = {k: EVIDENCE.get(k) for k in ["passed", "version", "nativeThreadId", "sourceExitCode",
                  "steps", "checkpointSHA256", "initialInstructionsPreserved", "providerRequestCensus",
                  "attachmentCensus", "error", "limitations"]}
        report["sourceDigests"] = {str(p.relative_to(ROOT)): hashlib.sha256(p.read_bytes()).hexdigest() for p in sources}
        report["helperSHA256"] = hashlib.sha256(HELPER.read_bytes()).hexdigest()
        report["privateEvidenceDirectory"] = str(OUT)
        REPORT.parent.mkdir(parents=True, exist_ok=True)
        REPORT.write_text(json.dumps(report, indent=2) + "\n")


if __name__ == "__main__":
    main()
