"""Publish a bounded answer from one stock Hermes CLI run; never own its agent loop."""

import argparse
import json
import os
from pathlib import Path
import signal
import selectors
import subprocess
import sys
import time

MAX_EVENT_BYTES = 1024 * 1024
MAX_RESULT_BYTES = 3584


def cleanup_group(pid):
    # The adapter is the Pod's PID 1. Reap adopted children and stop any native
    # tool process still alive after the stock CLI has returned.
    for sig in (signal.SIGTERM, signal.SIGKILL):
        try:
            os.killpg(pid, sig)
        except ProcessLookupError:
            pass
        until = time.monotonic() + 0.25
        while True:
            try:
                reaped, _ = os.waitpid(-1, os.WNOHANG)
            except ChildProcessError:
                return
            if reaped:
                continue
            if time.monotonic() >= until:
                break
            time.sleep(0.01)


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("Duplicate JSON member")
        result[key] = value
    return result


def load_json(value):
    return json.loads(value, object_pairs_hook=unique_object,
                      parse_constant=lambda _: (_ for _ in ()).throw(ValueError("Invalid number")))


def result_document(run_uid, text):
    if not isinstance(run_uid, str) or not run_uid or not isinstance(text, str):
        raise ValueError("Invalid native result")
    # Validate Unicode before encoding so surrogate text fails, never masquerades as UTF-8.
    text.encode("utf-8")
    result = {"version": "sproozi.run-result/v1", "runUID": run_uid,
              "result": {"text": text, "truncated": False}}
    def encode():
        return json.dumps(result, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    document = encode()
    if len(document) <= MAX_RESULT_BYTES:
        return document
    result["result"]["truncated"] = True
    low, high = 0, len(text)
    while low < high:
        middle = (low + high + 1) // 2
        result["result"]["text"] = text[:middle]
        if len(encode()) <= MAX_RESULT_BYTES:
            low = middle
        else:
            high = middle - 1
    result["result"]["text"] = text[:low]
    document = encode()
    if len(document) > MAX_RESULT_BYTES:
        raise ValueError("Run identity exceeds result limit")
    return document


def native_result(line):
    event = load_json(line)
    if not isinstance(event, dict) or not isinstance(event.get("type"), str):
        raise ValueError("Invalid native event")
    if event["type"] != "result":
        return None
    if type(event.get("exit_code")) is not int or event["exit_code"] != 0:
        raise ValueError("Native result failed")
    if "error" in event or not isinstance(event.get("text"), str):
        raise ValueError("Invalid native result")
    return event["text"]


def launch(args):
    contract = Path(args.contract)
    run_uid = load_json((contract / "input.json").read_text(encoding="utf-8"))["run"]["uid"]
    config = load_json((contract / "hermes-mcp.json").read_text(encoding="utf-8"))
    if set(config) != {"mcp_servers"} or not isinstance(config["mcp_servers"], dict):
        raise ValueError("Invalid native MCP configuration")
    if any(not name.startswith("sproozi-mcp-") for name in config["mcp_servers"]):
        raise ValueError("Native MCP alias may collide with a built-in toolset")
    if args.kubernetes_mcp:
        config["mcp_servers"]["sproozi-kubernetes"] = {
            "url": "https://kubernetes.default.svc/mcp", "enabled": True,
            "strict_redirect_headers": True, "connect_timeout": 15, "timeout": 60}
    home = Path(os.environ["HERMES_HOME"])
    home.mkdir(parents=True, exist_ok=True)
    # The runtime supplies an empty, disposable home; never inherit operator configuration.
    config_path = home / "config.yaml"
    config.update({"model": {"default": args.model, "provider": "openai-api",
                             "base_url": "https://api.openai.com/v1"},
                   "security": {"allow_lazy_installs": False},
                   "agent": {"max_turns": 16,
                             "disabled_toolsets": [] if config["mcp_servers"] else ["all"]},
                   "checkpoints": {"enabled": False}})
    with config_path.open("x", encoding="utf-8") as output:
        json.dump(config, output)
    env = dict(os.environ)
    env["HERMES_EPHEMERAL_SYSTEM_PROMPT"] = (contract / "instructions.txt").read_text(encoding="utf-8")
    env["OPENAI_API_KEY"] = "sproozi-local-placeholder"
    env["OPENAI_BASE_URL"] = "https://api.openai.com/v1"
    # Prevent ambient hosted/worker modes from changing native exit or provider selection.
    for name in ("HERMES_KANBAN_TASK", "HERMES_GUEST_ONBOARDING", "HERMES_TURN_AUTHOR"):
        env.pop(name, None)
    toolsets = ",".join(sorted(config["mcp_servers"])) or "all"
    command = [args.hermes, "chat", "--oneshot", "--query-file", str(contract / "request.json"),
               "--format", "stream-json", "--provider", "openai-api", "--model", args.model,
               "--max-turns", "16", "--ignore-rules", "--toolsets", toolsets]
    child = None
    received_signal = None
    def forward(sig, _frame):
        nonlocal received_signal
        received_signal = sig
        if child is None:
            return
        try:
            os.killpg(child.pid, sig)
        except ProcessLookupError:
            pass
    signal.signal(signal.SIGTERM, forward)
    signal.signal(signal.SIGINT, forward)
    if received_signal is not None:
        return 128 + received_signal
    child = subprocess.Popen(command, env=env, stdout=subprocess.PIPE, start_new_session=True)
    if received_signal is not None:
        forward(received_signal, None)
    answer = None
    invalid = False
    pending = bytearray()
    def consume(line):
        nonlocal answer, invalid
        sys.stdout.buffer.write(line)
        sys.stdout.buffer.flush()
        if len(line) > MAX_EVENT_BYTES:
            invalid = True
            return
        if not line.strip():
            return
        try:
            if answer is not None:
                raise ValueError("Event follows terminal result")
            answer = native_result(line)
        except (ValueError, UnicodeError):
            invalid = True

    descriptor = child.stdout.fileno()
    os.set_blocking(descriptor, False)
    reader = selectors.DefaultSelector()
    reader.register(descriptor, selectors.EVENT_READ)
    eof = False
    tail_bytes = 0
    forced_stop = False
    try:
        # Native process exit, not inherited-pipe EOF, owns completion. A tool
        # grandchild may retain stdout after the CLI returns. Drain only data
        # immediately available after exit, with a bounded tail, then clean up.
        while True:
            exited = child.poll() is not None
            ready = reader.select(0 if exited else 0.05) if not eof else []
            if eof and not exited:
                time.sleep(0.05)
            if exited and not ready:
                break
            if not ready:
                continue
            try:
                chunk = os.read(descriptor, 65536)
            except BlockingIOError:
                continue
            if not chunk:
                eof = True
                reader.unregister(descriptor)
                continue
            if exited:
                tail_bytes += len(chunk)
                if tail_bytes > MAX_EVENT_BYTES:
                    invalid = True
                    break
            pending.extend(chunk)
            while True:
                newline = pending.find(b"\n")
                if newline < 0:
                    break
                consume(bytes(pending[:newline + 1]))
                del pending[:newline + 1]
            if len(pending) > MAX_EVENT_BYTES:
                invalid = True
                forced_stop = child.poll() is None
                # Stop oversized native output even when a child ignores TERM.
                for sig in (signal.SIGTERM, signal.SIGKILL):
                    try:
                        os.killpg(child.pid, sig)
                    except ProcessLookupError:
                        pass
                break
        if pending:
            consume(bytes(pending))
        code = child.wait()
    finally:
        reader.close()
        child.stdout.close()
        cleanup_group(child.pid)
    if received_signal is not None:
        return 128 + received_signal
    if forced_stop:
        return 1
    if code != 0:
        return code if code > 0 else 128 - code
    if invalid or answer is None:
        raise ValueError("Missing or invalid native result")
    document = result_document(run_uid, answer)
    # Kubelet provides a mounted file: write in place; renaming over it is invalid.
    with open(args.result, "wb") as output:
        output.write(document)
    return 0


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", required=True)
    parser.add_argument("--kubernetes-mcp", action="store_true",
                        help="Trusted opt-in for the gateway native Kubernetes Pod-list tool")
    parser.add_argument("--contract", default="/etc/sproozi/contract")
    parser.add_argument("--result", default="/dev/termination-log")
    parser.add_argument("--hermes", default="/opt/hermes/.venv/bin/hermes")
    args = parser.parse_args()
    try:
        return launch(args)
    except (OSError, ValueError, KeyError, TypeError, UnicodeError):
        print("Could not publish the native Hermes run result", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
