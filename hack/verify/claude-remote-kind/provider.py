#!/usr/bin/env python3
"""Deterministic Anthropic Messages fixture for the Claude remote-executor proof.

It runs in the proof cluster, behind the shared gateway's ANTHROPIC_UPSTREAM_URL,
so no host address has to be reachable from Kind. It uses only the standard
library, as the native image's Python runs it. Each conversation turn is chosen
by the marker in its newest prompt; the reply is scripted, and requests that
lack the history a turn requires are refused, so a resumed conversation that
lost its history fails. Every request is recorded, without bodies but with
the time it arrived, in /tmp/fixture/requests.json for the driver to read
with kubectl exec.

The fixture accepts the public placeholder key the gateway holds, or the public
placeholder OAuth access token of a fixture sign-in (ADR 0047). Neither is a
real credential.
"""
import json
import os
from pathlib import Path
import ssl
import sys
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

MODEL = "claude-sonnet-4-5"
API_KEY = "claude-remote-fixture-placeholder"
OAUTH_TOKEN = "sk-ant-oat01-claude-remote-fixture-sign-in"
PROFILE_SKILL = "remote-check"
PROFILE_MARKER = "PROFILE_MARKER_CLAUDE_REMOTE"
DIAGNOSE = "mcp__sproozi__diagnose"
# The file only an uploaded workspace holds, and the Bash call that prints it.
UPLOAD_FILE = "upload-marker.txt"
UPLOAD_MARKER = "UPLOAD_MARKER=teal-otter-845"
UPLOAD_COMMAND = {"command": "cat " + UPLOAD_FILE, "description": "Print the uploaded marker"}

# Each scenario: the prompt the driver types (or submits as a task), the final
# reply, and the earlier prompts and replies its request must carry.
SCENARIOS = {
    "first": {"prompt": "Remember the remote fixture fact amber-heron-204.",
              "answer": "CLAUDE_REMOTE_FIRST_COMPLETE", "history": []},
    "resumed": {"prompt": "Continue this remote conversation using the fixture fact.",
                "answer": "CLAUDE_REMOTE_RESUMED_COMPLETE",
                "history": ["Remember the remote fixture fact amber-heron-204.", "CLAUDE_REMOTE_FIRST_COMPLETE"]},
    "upload": {"prompt": "Use Bash to print the uploaded workspace marker file.",
               "answer": "CLAUDE_REMOTE_UPLOAD_COMPLETE", "history": []},
    "exit": {"prompt": "Answer once before this visit exits.",
             "answer": "CLAUDE_REMOTE_EXIT_TURN_COMPLETE", "history": []},
    "task": {"prompt": "Report the remote Claude task marker cobalt-wren-517.",
             "answer": "CLAUDE_REMOTE_TASK_RESULT cobalt-wren-517", "history": []},
    "tools": {"prompt": "Call the sproozi diagnose tool, then load the remote-check profile skill, then report.",
              "answer": "CLAUDE_REMOTE_TOOLS_COMPLETE", "history": []},
    "refresh-before": {"prompt": "Answer while the sandbox still holds its first gateway token.",
                       "answer": "CLAUDE_REMOTE_REFRESH_BEFORE_COMPLETE", "history": []},
    "refresh-after": {"prompt": "Answer now that the sandbox's first gateway token has expired.",
                      "answer": "CLAUDE_REMOTE_REFRESH_AFTER_COMPLETE",
                      "history": ["Answer while the sandbox still holds its first gateway token.",
                                  "CLAUDE_REMOTE_REFRESH_BEFORE_COMPLETE"]},
}
# Claude Code connects MCP servers after its first request, so the tools turn
# waits with Bash until the diagnose tool is offered, at most this often.
MAX_WAITS = 6
WAIT_COMMAND = {"command": "sleep 2; echo waited", "description": "Wait for the MCP connection"}
AUXILIARY_LIMIT = 1 << 20


class Refused(ValueError):
    """A request the fixture does not script, recorded and answered 400."""


def text_of(content):
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        return "\n".join(block.get("text", "") for block in content
                         if isinstance(block, dict) and block.get("type") == "text")
    return ""


def prompt_index(body):
    """The newest user message holding a scenario's prompt, and its scenario.

    A loaded skill's body arrives as a later user message, so the turn's
    prompt is not always the newest one."""
    messages = body.get("messages", [])
    for index in range(len(messages) - 1, -1, -1):
        m = messages[index]
        if m.get("role") != "user":
            continue
        text = text_of(m.get("content"))
        matches = [name for name, s in SCENARIOS.items() if s["prompt"] in text]
        if len(matches) > 1:
            raise Refused("ambiguous prompt")
        if matches:
            return index, matches[0]
    raise Refused("unscripted prompt")


def scenario_of(body):
    return prompt_index(body)[1]


def turn_blocks(body):
    """The tool calls and results since the turn's prompt."""
    messages = body.get("messages", [])
    start = prompt_index(body)[0]
    calls, results = [], []
    for message in messages[start:]:
        content = message.get("content")
        if not isinstance(content, list):
            continue
        for block in content:
            if not isinstance(block, dict):
                continue
            if block.get("type") == "tool_use":
                calls.append(block)
            elif block.get("type") == "tool_result":
                results.append(block)
    return calls, results


def result_text(result):
    content = result.get("content")
    return content if isinstance(content, str) else text_of(content) or json.dumps(content)


def results_for(calls, results, name):
    """The results of the calls to name, matched by tool use ID."""
    ids = {c.get("id") for c in calls if c.get("name") == name}
    return [r for r in results if r.get("tool_use_id") in ids]


def offered(body):
    return {t.get("name") for t in body.get("tools", []) if isinstance(t, dict)}


def plan(body):
    """Classify one Messages request and choose its reply.

    Returns (record, reply), where reply is ("text", str) or
    ("tool", name, input). The record carries no request body.
    """
    if body.get("model") != MODEL:
        # The terminal's auxiliary calls (titles, Bash prefixes, file paths)
        # carry no conversation; answer them without scripting.
        system = text_of(body.get("system"))
        if "new conversation topic" in system:
            answer = '{"isNewTopic":false,"title":null}'
        elif "process Bash commands" in system:
            answer = "none"
        else:
            answer = "FIXTURE_AUXILIARY"
        return {"kind": "auxiliary", "model": str(body.get("model"))[:64]}, ("text", answer)
    name = scenario_of(body)
    scenario = SCENARIOS[name]
    encoded = json.dumps(body)
    record = {"kind": "conversation", "scenario": name,
              "missingHistory": [item for item in scenario["history"] if item not in encoded]}
    if record["missingHistory"]:
        raise Refused("missing history")
    calls, results = turn_blocks(body)
    names = [c.get("name") for c in calls]
    tools = offered(body)
    if name == "upload":
        if "Bash" not in names:
            if "Bash" not in tools:
                raise Refused("Bash not offered")
            record["phase"] = "read-upload"
            return record, ("tool", "Bash", UPLOAD_COMMAND)
        printed = results_for(calls, results, "Bash")
        record.update(phase="answer", uploadMarkerSeen=any(UPLOAD_MARKER in result_text(r) for r in printed))
        return record, ("text", scenario["answer"])
    if name != "tools":
        record["phase"] = "answer"
        return record, ("text", scenario["answer"])
    record.update(diagnoseOffered=DIAGNOSE in tools, skillOffered="Skill" in tools,
                  toolErrors=sum(1 for r in results if r.get("is_error")))
    if DIAGNOSE not in names:
        if DIAGNOSE in tools:
            record["phase"] = "diagnose"
            return record, ("tool", DIAGNOSE, {})
        if names.count("Bash") >= MAX_WAITS:
            raise Refused("diagnose never offered")
        record["phase"] = "wait"
        return record, ("tool", "Bash", WAIT_COMMAND)
    diagnose = results_for(calls, results, DIAGNOSE)
    record["diagnoseResult"] = bool(diagnose) and not diagnose[-1].get("is_error")
    if "Skill" not in names:
        if "Skill" not in tools:
            raise Refused("Skill not offered")
        record["phase"] = "skill"
        return record, ("tool", "Skill", {"skill": "profile:" + PROFILE_SKILL})
    record.update(phase="answer", profileSkillLoaded=PROFILE_MARKER in encoded)
    return record, ("text", scenario["answer"])


def message(body, reply, index):
    if reply[0] == "tool":
        block = {"type": "tool_use", "id": "toolu_remote_%d" % index, "name": reply[1], "input": reply[2]}
        stop = "tool_use"
    else:
        block, stop = {"type": "text", "text": reply[1]}, "end_turn"
    return {"id": "msg_remote_%d" % index, "type": "message", "role": "assistant", "model": body.get("model"),
            "content": [block], "stop_reason": stop, "stop_sequence": None,
            "usage": {"input_tokens": 32, "output_tokens": 8}}


def events(msg):
    """The SSE events that stream msg."""
    block = msg["content"][0]
    tool = block["type"] == "tool_use"
    yield "message_start", {"message": {**msg, "content": [], "stop_reason": None,
                                        "usage": {"input_tokens": 32, "output_tokens": 0}}}
    yield "content_block_start", {"index": 0, "content_block": {**block, **({"input": {}} if tool else {"text": ""})}}
    yield "content_block_delta", {"index": 0, "delta": (
        {"type": "input_json_delta", "partial_json": json.dumps(block["input"])} if tool
        else {"type": "text_delta", "text": block["text"]})}
    yield "content_block_stop", {"index": 0}
    yield "message_delta", {"delta": {"stop_reason": msg["stop_reason"], "stop_sequence": None},
                            "usage": {"output_tokens": 8}}
    yield "message_stop", {}


class Recorder:
    def __init__(self, path):
        self.path, self.lock, self.records = path, threading.Lock(), []

    def add(self, record):
        with self.lock:
            if len(self.records) >= 512:
                raise Refused("request bound")
            # When the request reached the fixture, by the cluster's clock.
            record.setdefault("at", round(time.time(), 3))
            self.records.append(record)
            self.persist()
            return len(self.records) - 1

    def update(self, index, **fields):
        with self.lock:
            self.records[index].update(fields)
            self.persist()

    def persist(self):
        temporary = self.path.with_suffix(".tmp")
        temporary.write_text(json.dumps({"requests": self.records}))
        os.replace(temporary, self.path)


def handler(recorder):
    class Provider(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_GET(self):
            recorder.add({"kind": "refused", "path": urlsplit(self.path).path[:128], "method": "GET"})
            self.send_error(404)

        def do_POST(self):
            path = urlsplit(self.path).path
            signed_in = self.headers.get("Authorization") == "Bearer " + OAUTH_TOKEN
            if self.headers.get("x-api-key") != API_KEY and not signed_in:
                recorder.add({"kind": "refused", "path": path[:128], "reason": "credential"})
                self.send_error(403, "Only the fixture placeholder credentials are accepted")
                return
            size = int(self.headers.get("Content-Length", "0"))
            if not 0 < size <= AUXILIARY_LIMIT:
                self.send_error(413)
                return
            body = json.loads(self.rfile.read(size))
            if path == "/v1/messages/count_tokens":
                self.reply_json({"input_tokens": 32})
                return
            if path != "/v1/messages":
                recorder.add({"kind": "refused", "path": path[:128], "method": "POST"})
                self.send_error(404)
                return
            try:
                record, reply = plan(body)
            except Refused as refusal:
                recorder.add({"kind": "refused", "path": path, "reason": str(refusal)})
                self.send_error(400, str(refusal))
                return
            record.update(auth="sign-in" if signed_in else "key", completed=False)
            index = recorder.add(record)
            msg = message(body, reply, index)
            if not body.get("stream"):
                self.reply_json(msg)
            else:
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Connection", "close")
                self.end_headers()
                for kind, data in events(msg):
                    self.wfile.write(("event: %s\ndata: %s\n\n" % (kind, json.dumps({"type": kind, **data}))).encode())
                    self.wfile.flush()
            recorder.update(index, completed=True)

        def reply_json(self, value):
            data = json.dumps(value).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
    return Provider


def main():
    directory = Path(os.environ.get("FIXTURE_DIRECTORY", "/tmp/fixture"))
    directory.mkdir(parents=True, exist_ok=True)
    recorder = Recorder(directory / "requests.json")
    recorder.persist()
    server = ThreadingHTTPServer(("0.0.0.0", int(os.environ.get("FIXTURE_PORT", "8443"))), handler(recorder))
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.minimum_version = ssl.TLSVersion.TLSv1_2
    tls.load_cert_chain(os.environ.get("FIXTURE_CERT", "/tls/tls.crt"), os.environ.get("FIXTURE_KEY", "/tls/tls.key"))
    server.socket = tls.wrap_socket(server.socket, server_side=True)
    server.daemon_threads = True
    print("Claude remote fixture listening", file=sys.stderr, flush=True)
    server.serve_forever()


if __name__ == "__main__":
    main()
