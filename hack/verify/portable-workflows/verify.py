#!/usr/bin/env python3
"""Prove remote native task retention and fresh home continuation in owned fixtures."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import ssl
import subprocess
import time
import urllib.error
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[3]
spec = importlib.util.spec_from_file_location("mobility_host", ROOT / "hack/verify/session-mobility/host.py")
HOST = importlib.util.module_from_spec(spec)
spec.loader.exec_module(HOST)


def check(value, message):
    if not value:
        raise RuntimeError(message)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("home-config", "profile-config", "session-binary", "pty-helper", "output-directory"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--request-id", help="Retained original submission UUID for recovery proof")
    args = parser.parse_args()
    os.umask(0o077)
    out = HOST.private_directory(args.output_directory)
    check(not (out / "report.json").exists(), "Use a fresh proof output directory")
    home = json.loads(args.home_config.read_text())
    profile = json.loads(args.profile_config.read_text())
    check(Path(home["gateway"]["providerTokenFile"]).read_text().strip() == HOST.TOKEN,
          "This verifier accepts only the deterministic public provider fixture")
    source = profile["placementName"]
    origin = home["remote"][source]["URL"]
    tls = ssl.create_default_context(cafile=home["remote"][source]["CAFile"])
    task_token = Path(profile["workTokenFile"]).read_text().strip()
    socket = Path(home["directory"]) / "control.sock"
    check(not socket.exists(), "First remote task must run without the home daemon")
    report = {"passed": False, "scope": "genuine stock native task via authenticated MCP; remote retention and fresh local resume",
              "limitations": ["Deterministic provider; no paid inference or real engineering contribution claim",
                              "MCP caller emulates Hermes's supported native client, not a fresh Hermes harness proof",
                              "Text/Git fixture; personal infrastructure is never accessed"], "steps": []}
    def save():
        HOST.write(out / "report.json", report)
    def post(path, body, token=task_token):
        request = urllib.request.Request(origin + path, data=json.dumps(body).encode(), method="POST",
                                         headers={"Authorization": "Bearer " + token, "Content-Type": "application/json",
                                                  "Accept": "application/json, text/event-stream"})
        with urllib.request.urlopen(request, context=tls, timeout=45) as response:
            return json.loads(response.read())
    def tool(name, arguments, call_id):
        reply = post("/work/mcp", {"jsonrpc": "2.0", "id": call_id, "method": "tools/call",
                                   "params": {"name": name, "arguments": arguments}})
        check("error" not in reply and not reply["result"].get("isError"), "MCP tool failed")
        return reply["result"]["structuredContent"]
    save()
    initialized = post("/work/mcp", {"jsonrpc": "2.0", "id": 1, "method": "initialize",
                                    "params": {"protocolVersion": "2025-03-26", "capabilities": {},
                                               "clientInfo": {"name": "portable-workflow-proof", "version": "1"}}})
    check("result" in initialized, "MCP initialization failed")
    intent = {"requestId": args.request_id or str(uuid.uuid4()), "harness": "codex", "repository": "sproozi",
              "executor": source, "task": HOST.PROMPTS[0]}
    report["requestId"] = intent["requestId"]
    save()
    view = tool("sproozi_run", intent, 2)
    report["sessionId"] = view["id"]
    report["executionId"] = view["executionId"]
    save()
    replay = tool("sproozi_run", intent, 3)
    check(replay["id"] == view["id"] and replay["executionId"] == view["executionId"], "Submission replay launched another execution")
    deadline = time.monotonic() + 240
    number = 4
    while time.monotonic() < deadline:
        current = tool("sproozi_status", {"id": view["id"]}, number)
        number += 1
        if current["phase"] == "retained":
            break
        check(current["phase"] not in ("unknown", "cancelled"), "Remote execution needs recovery")
        time.sleep(0.5)
    else:
        raise RuntimeError("Remote task did not retain its checkpoint before timeout")
    check(current["threadId"] and current["result"] == HOST.ANSWERS[0], "Genuine native answer missing")
    report["remote"] = current
    report["steps"].append("Remote initial native task completed and retained while home was absent")
    save()
    with (out / "home.log").open("wb") as log:
        daemon = subprocess.Popen([str(args.session_binary.resolve()), "serve", "--config", str(args.home_config.resolve())],
                                  cwd=ROOT, stdout=log, stderr=log)
        report["homePID"] = daemon.pid
        save()
        try:
            ready = time.monotonic() + 15
            while not socket.exists():
                check(daemon.poll() is None and time.monotonic() < ready, "Home daemon did not start")
                time.sleep(0.1)
            def command(*words):
                completed = subprocess.run([str(args.session_binary.resolve()), *words, "--socket", str(socket)],
                                           cwd=ROOT, capture_output=True, text=True, timeout=120)
                check(completed.returncode == 0, completed.stderr[-1024:])
                return json.loads(completed.stdout)
            pulled = command("pull", view["id"], "--from", source)
            check(pulled["threadId"] == current["threadId"], "Pull changed native identity")
            after = tool("sproozi_status", {"id": view["id"]}, number)
            number += 1
            check(after["phase"] == "home-owned", "Remote owner did not commit exclusive handoff")
            resumed = command("resume", view["id"], "--detach")
            check(resumed["productId"] == view["id"] and resumed["threadId"] == current["threadId"], "Resume lost product/native identity")
            check(resumed["deadline"] > current["deadline"], "Later local execution did not receive a fresh deadline")
            report["local"] = resumed
            report["steps"].append("Claimed checkpoint resumed in local Docker under a fresh execution identity and deadline")
            save()
            def terminal(index, name):
                with (out / (name + ".stdout")).open("w") as stdout, (out / (name + ".stderr")).open("w") as stderr:
                    subprocess.run(["python3", str(ROOT / "hack/verify/session-mobility/host.py"), "terminal",
                            "--session-binary", str(args.session_binary.resolve()), "--pty-helper", str(args.pty_helper.resolve()),
                            "--socket", str(socket), "--session-id", view["id"], "--output-directory", str(out / name),
                            "--prompt-index", str(index)], check=True, timeout=150, cwd=ROOT, stdout=stdout, stderr=stderr)
            terminal(1, "terminal")
            terminal(3, "native-tool")
            stopped = command("stop", view["id"])
            check(stopped["initialStage"] == "retained" and stopped["threadId"] == current["threadId"], "Local follow-up did not retain native state")
            report["stopped"] = stopped
            report["steps"].append("Genuine same-conversation local follow-up completed and checkpointed without another writer")
            report["steps"].append("Stock native exec_command wrote and read the controlled workspace file")
            report["passed"] = True
            save()
        finally:
            try:
                if "local" in report and "stopped" not in report and daemon.poll() is None:
                    operation = str(uuid.uuid4())
                    report["cleanup"] = {"operation": operation}
                    save()
                    cleanup = subprocess.run([str(args.session_binary.resolve()), "cancel", view["id"],
                                              "--socket", str(socket), "--operation", operation],
                                             capture_output=True, text=True, timeout=120, cwd=ROOT)
                    report["cleanup"].update(exit=cleanup.returncode, stderr=cleanup.stderr[-1024:])
                    save()
            finally:
                daemon.terminate()
                try:
                    daemon.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    daemon.kill()
                    daemon.wait()
    print(json.dumps({"passed": report["passed"], "report": str(out / "report.json")}))


if __name__ == "__main__":
    main()
