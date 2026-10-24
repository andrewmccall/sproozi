#!/usr/bin/env python3
"""Deterministic Copilot fixture for the local verifiers (ADR 0048).

serve runs an OpenAI Responses provider for Copilot in bring-your-own-key
mode: it answers each fixture prompt with its marker and records, per model
request, whether it carried the gateway's key and the instructions it saw.
terminal drives the stock Copilot TUI through production `sproozi attach`:
it submits one prompt, waits for the answer and detaches.
"""

import argparse
import importlib.util
import json
import os
from pathlib import Path
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

HERE = Path(__file__).resolve().parent


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    loaded = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(loaded)
    return loaded


HOST = module("copilot_fixture_host", HERE / "host.py")
MODEL = "gpt-fixture"
# The gateway-held key the fixture expects; Copilot itself sends a placeholder.
KEY = "tui-placeholder"
PROMPTS = ["Remember the fixture fact amber-heron-204.", "What was the fixture fact?"]
ANSWERS = ["COPILOT_FIRST_COMPLETE", "COPILOT_RESUMED_COMPLETE"]


def answer(body):
    """The marker for the newest fixture prompt in the request."""
    text = json.dumps(body.get("input", []))
    found = [i for i, prompt in enumerate(PROMPTS) if prompt in text]
    return ANSWERS[max(found)] if found else "COPILOT_UNKNOWN_PROMPT"


def serve(args):
    out = HOST.private_directory(args.output_directory)
    records, lock = [], threading.Lock()

    class Handler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def send(self, code, data, kind="application/json"):
            self.send_response(code)
            self.send_header("Content-Type", kind)
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self):
            if self.path.rstrip("/").endswith("/models"):
                return self.send(200, json.dumps({"object": "list", "data": [{"id": MODEL, "object": "model"}]}).encode())
            self.send(404, b"{}")

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))) or b"{}")
            with lock:
                records.append({"path": self.path, "model": body.get("model"),
                                "gatewayKey": self.headers.get("Authorization") == "Bearer " + KEY,
                                "signedIn": False, "instructions": body.get("instructions", ""),
                                "input": json.dumps(body.get("input", []))})
                HOST.write(out / "requests.json", records)
            if not self.path.endswith("/responses"):
                return self.send(404, b"{}")
            text = answer(body)
            item = {"id": "msg_fixture", "type": "message", "status": "completed", "role": "assistant",
                    "content": [{"type": "output_text", "text": text, "annotations": []}]}
            response = {"id": "resp_fixture", "object": "response", "created_at": int(time.time()), "model": MODEL,
                        "status": "completed", "output": [item],
                        "usage": {"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}}
            events = [("response.created", {"response": {**response, "status": "in_progress", "output": []}}),
                      ("response.output_item.added", {"output_index": 0, "item": item}),
                      ("response.output_item.done", {"output_index": 0, "item": item}),
                      ("response.completed", {"response": response})]
            stream = "".join("data: " + json.dumps({"type": kind, **fields}) + "\n\n" for kind, fields in events)
            self.send(200, stream.encode(), "text/event-stream")

    HOST.write(out / "requests.json", records)
    ThreadingHTTPServer((args.listen, args.port), Handler).serve_forever()


def terminal(args):
    out = HOST.private_directory(args.output_directory)
    tui = HOST.TUI
    tui.OUT, tui.HELPER = out, args.pty_helper.resolve()
    home = HOST.private_directory(out / "cli-home")
    env = {"PATH": os.environ["PATH"], "HOME": str(home), "TERM": "xterm-256color",
           "COLORTERM": "truecolor", "LANG": "en_US.UTF-8"}
    command = [str(args.session_binary.resolve()), "attach", "--socket", str(args.socket), "--id", args.session_id]
    evidence = {"passed": False, "harness": "copilot", "promptIndex": args.prompt_index,
                "path": "actual production sproozi attach and stock Copilot TUI", "command": command}
    supervisor = tui.Supervisor(command, out, env, "attachment", out)
    try:
        connection = supervisor.connect()
        if not supervisor.wait_text("Copilot", args.timeout):
            raise RuntimeError("Stock Copilot TUI did not render through attach")
        time.sleep(1)
        tui.submit(connection, PROMPTS[args.prompt_index])
        if not supervisor.wait_text(ANSWERS[args.prompt_index], args.timeout):
            raise RuntimeError("Copilot's answer was not rendered")
        supervisor.command(b"\x1d")
        end = time.monotonic() + 10
        while time.monotonic() < end and not supervisor.rpc("status").get("exited"):
            time.sleep(0.05)
        if not supervisor.rpc("status").get("exited") or supervisor.wait(1) != 0:
            raise RuntimeError("sproozi attach did not detach successfully")
        evidence.update(passed=True, cliExitCode=supervisor.returncode, terminalBytes=len(supervisor.data),
                        limitation="Answer text is diagnostic; the stop independently checks Copilot's journal")
    finally:
        HOST.write(out / "terminal-evidence.json", evidence)
        supervisor.close()
    print(json.dumps(evidence))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    serving = commands.add_parser("serve")
    serving.add_argument("--output-directory", type=Path, required=True)
    serving.add_argument("--listen", default="127.0.0.1")
    serving.add_argument("--port", type=int, required=True)
    attaching = commands.add_parser("terminal")
    for name in ("--session-binary", "--pty-helper", "--socket", "--output-directory"):
        attaching.add_argument(name, type=Path, required=True)
    attaching.add_argument("--session-id", required=True)
    attaching.add_argument("--prompt-index", type=int, default=0)
    attaching.add_argument("--timeout", type=float, default=120)
    args = parser.parse_args()
    {"serve": serve, "terminal": terminal}[args.command](args)


if __name__ == "__main__":
    main()
