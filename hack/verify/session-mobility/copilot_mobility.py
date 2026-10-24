#!/usr/bin/env python3
"""Credential-free Copilot mobility fixture and public-CLI proof (ADR 0048).

The Claude follow-up's shape, for Copilot with a gateway-held key on the
OpenAI Responses route: prepare/serve/render are administrator helpers; verify
uses only the production session socket and checkpoint CLI. A completed
provider reply is not Copilot's completed turn or a writer fence: only the
production stop supplies those, from Copilot's journal and the placement.
"""
import argparse
import base64
import contextlib
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import signal
import ssl
import sys
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

HERE = Path(__file__).resolve().parent


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    loaded = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(loaded)
    return loaded


HOST = module("copilot_mobility_host", HERE / "host.py")
BASE = module("copilot_mobility_base", HERE / "verify.py")
KUBE = module("copilot_mobility_kube", HERE / "kubernetes.py")
MODEL = "gpt-fixture"
INSTRUCTIONS = "Only this deterministic local fixture is approved. INITIAL_COPILOT_222."
TOOL_MARKER = "TOOL_FACT=teal-otter-517"
TOOL_FILE = "copilot-tool-effect.txt"
TOOL_COMMAND = "printf 'TOOL_FACT=teal-otter-517\\n' > copilot-tool-effect.txt; cat copilot-tool-effect.txt"
PROMPTS = ["Remember the fixture fact amber-heron-204.",
           "Run the fixture command and report its output.",
           "What was the first fixture fact?"]
ANSWERS = ["COPILOT_FIRST_COMPLETE", "COPILOT_TOOL_COMPLETE", "COPILOT_RESUMED_COMPLETE"]
TEMPLATE = "mobility-copilot"
PORT = 18785
UPSTREAM = "https://192.168.65.254:%d" % PORT
UNITS_PER_REQUEST = 15


def prepare(args):
    out = HOST.private_directory(args.output_directory)
    BASE.check(not any(out.iterdir()), "Prepare requires an empty private directory")
    with contextlib.redirect_stdout(io.StringIO()):
        HOST.prepare(args)
    config = json.loads((out / "home-config.json").read_text())
    config["model"], config["local"]["Harness"] = MODEL, "copilot"
    config.pop("nativeOptions", None)
    HOST.write(out / "home-config.json", config)
    hints = json.loads((out / "profile-hints.json").read_text())
    hints.update(harness="copilot", model=MODEL, nativeOptions=[], providerURLFromKind=UPSTREAM,
                 prompts=PROMPTS, answers=ANSWERS,
                 limitations=["Deterministic Responses fixture, not a live OpenAI or GitHub service",
                              "Provider completion alone is not Copilot's completed turn or a writer fence"])
    hints.pop("extendedPrompts", None)
    hints.pop("extendedAnswers", None)
    HOST.write(out / "profile-hints.json", hints)
    print(json.dumps({"homeConfig": str(out / "home-config.json"), "hints": str(out / "profile-hints.json"),
                      "harness": "copilot"}))


def items(body):
    value = body.get("input", [])
    return value if isinstance(value, list) else []


def classify(body):
    entries = items(body)
    users = [e for e in entries if isinstance(e, dict) and e.get("role") == "user"]
    text = json.dumps(users[-1]) if users else ""
    matches = [i for i, prompt in enumerate(PROMPTS) if prompt in text]
    if len(matches) != 1:
        # Copilot can ask the model for more than a conversation turn; such a
        # request carries no fixture prompt and gets a fixed answer.
        return {"kind": "auxiliary", "turn": None, "phase": "answer", "missingHistory": []}
    turn = matches[0]
    encoded = json.dumps(entries)
    required = [PROMPTS[turn]] + [x for i in range(turn) for x in (PROMPTS[i], ANSWERS[i])]
    if turn >= 2:
        required.append(TOOL_MARKER)
    missing = [x for x in required if x not in encoded]
    if INSTRUCTIONS not in body.get("instructions", ""):
        missing.append("approved instructions")
    outputs = [e for e in entries if isinstance(e, dict) and e.get("type") == "function_call_output"]
    result = {"kind": "conversation", "turn": turn, "phase": "answer", "missingHistory": missing}
    if turn == 1 and not [o for o in outputs if TOOL_MARKER in json.dumps(o)]:
        if not any(t.get("name") == "bash" for t in body.get("tools", []) if isinstance(t, dict)):
            raise ValueError("Copilot's bash tool was not advertised")
        result["phase"] = "tool-call"
    elif turn == 1:
        result["toolOutputValidated"] = True
    return result


def reply(writer, body, request):
    if request["phase"] == "tool-call":
        item = {"id": "fc_fixture", "type": "function_call", "status": "completed", "call_id": "call_fixture_%d" % time.time_ns(),
                "name": "bash", "arguments": json.dumps({"command": TOOL_COMMAND, "description": "Write the fixture fact",
                                                         "mode": "sync", "initial_wait": 30})}
    else:
        text = ANSWERS[request["turn"]] if request["kind"] == "conversation" else "Fixture."
        item = {"id": "msg_fixture", "type": "message", "status": "completed", "role": "assistant",
                "content": [{"type": "output_text", "text": text, "annotations": []}]}
    response = {"id": "resp_fixture", "object": "response", "created_at": int(time.time()), "model": body.get("model"),
                "status": "completed", "output": [item], "usage": {"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}}
    events = [("response.created", {"response": {**response, "status": "in_progress", "output": []}}),
              ("response.output_item.added", {"output_index": 0, "item": item}),
              ("response.output_item.done", {"output_index": 0, "item": item}),
              ("response.completed", {"response": response})]
    for kind, fields in events:
        writer.wfile.write(("data: " + json.dumps({"type": kind, **fields}) + "\n\n").encode())
        writer.wfile.flush()


def serve(args):
    out = HOST.private_directory(args.output_directory)
    stop, lock, records = threading.Event(), threading.RLock(), []

    def persist():
        HOST.write(out / "requests.private.json", records)

    class Provider(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def json_reply(self, code, value):
            data = json.dumps(value).encode()
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(data)

        def do_GET(self):
            path = urlsplit(self.path).path
            if path == "/status":
                fields = ("kind", "turn", "phase", "completed", "missingHistory", "placeholderAuth",
                          "toolOutputValidated", "profileMarkers")
                with lock:
                    return self.json_reply(200, {"requests": [{k: r[k] for k in fields if k in r} for r in records]})
            if path.rstrip("/").endswith("/models"):
                return self.json_reply(200, {"object": "list", "data": [{"id": MODEL, "object": "model"}]})
            self.send_error(404)

        def do_POST(self):
            try:
                size = int(self.headers.get("Content-Length", "0"))
                if not 0 < size <= 4 << 20:
                    return self.send_error(413)
                body = json.loads(self.rfile.read(size))
                if self.headers.get("Authorization") != "Bearer " + HOST.TOKEN:
                    return self.send_error(403, "Only the public fixture credential is accepted")
                if urlsplit(self.path).path != "/v1/responses":
                    return self.send_error(404)
                if body.get("model") != MODEL:
                    raise ValueError("Unexpected model")
                request = classify(body)
                request.update(placeholderAuth=True, completed=False,
                               profileMarkers=BASE.profile_markers(json.dumps(body)))
                with lock:
                    if len(records) >= 128:
                        return self.send_error(429)
                    records.append(request)
                    persist()
                if request["missingHistory"]:
                    return self.send_error(400, "Required native history absent")
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream")
                self.send_header("Connection", "close")
                self.end_headers()
                reply(self, body, request)
                with lock:
                    request["completed"] = True
                    persist()
            except (ValueError, KeyError, TypeError) as error:
                self.send_error(400, type(error).__name__)

    server = ThreadingHTTPServer((args.listen, args.port), Provider)
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.minimum_version = ssl.TLSVersion.TLSv1_2
    tls.load_cert_chain(out / "gateway.crt", out / "gateway.key")
    server.socket = tls.wrap_socket(server.socket, server_side=True)
    server.daemon_threads = True
    persist()

    def shutdown(*_):
        stop.set()
        threading.Thread(target=server.shutdown, daemon=True).start()
    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)
    try:
        server.serve_forever()
    finally:
        server.server_close()


def render(args):
    import yaml
    hints = json.loads(args.hints.read_text())
    BASE.check(hints["harness"] == "copilot" and hints["providerToken"] == HOST.TOKEN and
               hints["providerURLFromKind"] == UPSTREAM and hints["nativeOptions"] == [] and hints["model"] == MODEL,
               "Only the configured Copilot fixture is permitted")
    validation = {**hints, "providerURLFromKind": KUBE.UPSTREAM}
    scratch = args.profile_config_output.parent / ("validate-" + uuid.uuid4().hex + ".json")
    HOST.write(scratch, validation)
    try:
        KUBE.hints_from(scratch)
    finally:
        scratch.unlink()
    BASE.check(KUBE.IMAGE.fullmatch(args.control_image) and KUBE.IMAGE.fullmatch(args.native_image),
               "Both images must be canonical repository@sha256 references")
    ca = args.ca_file.read_text()
    ssl.create_default_context(cadata=ca)
    trust = KUBE.trust_name(ca, "copilot-" + KUBE.TRUST)
    documents = [d for d in KUBE.build("config/default") if d and d["kind"] == "Deployment" and
                 d["metadata"]["name"] == "sproozi-gateway"]
    for doc in documents:
        KUBE.configure_deployment(doc, args.control_image, trust)
        for entry in doc["spec"]["template"]["spec"]["containers"][0]["env"]:
            if entry["name"] == "OPENAI_UPSTREAM_URL":
                entry["value"] = UPSTREAM
    resources = KUBE.fixture_resources(hints, args.native_image, ca, trust)
    for doc in resources:
        if doc["kind"] == "AgentRuntime":
            doc["spec"]["clientConfig"]["harness"] = "copilot"
            doc["spec"]["clientConfig"]["nativeSession"] = {"model": MODEL}
    documents.extend(resources)

    def substitute(value):
        if isinstance(value, list):
            return [substitute(v) for v in value]
        if isinstance(value, dict):
            return {k: substitute(v) for k, v in value.items()}
        return value.replace(KUBE.TEMPLATE, TEMPLATE) if isinstance(value, str) else value
    documents = substitute(documents)
    profile = KUBE.executor_profile(hints, principal="copilot-mobility-proof", harness="copilot",
                                    template=TEMPLATE, directory="/state/copilot-profile")
    documents.extend(KUBE.executor_resources(profile, args.native_image, ca))
    HOST.private_directory(args.profile_config_output.parent)
    HOST.write(args.profile_config_output, profile)
    sys.stdout.write(yaml.safe_dump_all(documents, sort_keys=False))


def terminal(args):
    out = HOST.private_directory(args.output_directory)
    tui = HOST.TUI
    tui.OUT, tui.HELPER = out, args.pty_helper.resolve()
    home = HOST.private_directory(out / "cli-home")
    env = {"PATH": os.environ["PATH"], "HOME": str(home), "TERM": "xterm-256color",
           "COLORTERM": "truecolor", "LANG": "en_US.UTF-8"}
    command = [str(args.session_binary.resolve()), "attach", "--socket", str(args.socket), "--id", args.session_id]
    evidence = {"passed": False, "harness": "copilot", "promptIndex": args.prompt_index, "command": command}
    supervisor = tui.Supervisor(command, out, env, "attachment", out)
    try:
        connection = supervisor.connect()
        BASE.check(supervisor.wait_text("Copilot", args.timeout), "Stock Copilot TUI did not render through attach")
        time.sleep(1)
        tui.submit(connection, PROMPTS[args.prompt_index])
        BASE.check(supervisor.wait_text(ANSWERS[args.prompt_index], args.timeout), "Copilot's answer was not rendered")
        supervisor.command(b"\x1d")
        end = time.monotonic() + 10
        while time.monotonic() < end and not supervisor.rpc("status").get("exited"):
            time.sleep(0.05)
        BASE.check(supervisor.rpc("status").get("exited") and supervisor.wait(1) == 0, "Attachment did not detach cleanly")
        evidence.update(passed=True, cliExitCode=supervisor.returncode, terminalBytes=len(supervisor.data))
    finally:
        HOST.write(out / "terminal-evidence.json", evidence)
        supervisor.close()
    print(json.dumps(evidence))


class Proof(BASE.Proof):
    def __init__(self, args):
        self.args, self.number, self.checkpoint_binary = args, 0, args.checkpoint_binary
        self.out = HOST.private_directory(args.output_directory)
        BASE.check(not (self.out / "report.json").exists(), "Use a fresh proof directory")
        self.config = json.loads(args.home_config.read_text())
        self.fixture = args.home_config.resolve().parent
        hints = json.loads((self.fixture / "profile-hints.json").read_text())
        BASE.check(self.config["local"]["Harness"] == "copilot" and self.config["model"] == MODEL and
                   hints["prompts"] == PROMPTS and hints["answers"] == ANSWERS and
                   Path(self.config["gateway"]["providerTokenFile"]).read_text().strip() == HOST.TOKEN,
                   "Only the configured public Copilot fixture configuration is allowed")
        self.socket = Path(self.config["directory"]) / "control.sock"
        self.id = str(uuid.uuid4())
        self.env = {"PATH": os.environ["PATH"], "HOME": str(self.out), "LANG": "C.UTF-8",
                    "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull, "GIT_TERMINAL_PROMPT": "0"}
        if os.environ.get("KUBECONFIG"):
            self.env["KUBECONFIG"] = os.environ["KUBECONFIG"]
        self.report = {"passed": False, "harness": "copilot", "sessionId": self.id, "steps": [],
                       "fixtureCredentialIsPublicPlaceholder": True,
                       "scope": "stock Copilot TUI and actual bash through production session placements",
                       "limitations": ["Deterministic Responses provider; no paid OpenAI request",
                                       "Containment controls require separate administrator evidence"]}
        self.save()

    def workspace(self, repo):
        result = super().workspace(repo)
        if (repo / TOOL_FILE).exists():
            result["files"][TOOL_FILE] = BASE.digest((repo / TOOL_FILE).read_bytes())
        return result

    def validate_record(self, status, deadline, thread=None, placement=None):
        record = super().validate_record(status, deadline, thread, placement)
        launches = [v["launch"] for v in record["visits"]] + ([record["active"]["launch"]] if record.get("active") else [])
        BASE.check(record["harness"] == "copilot" and all(l["harness"] == "copilot" and
                   l["instructionDigest"] == self.config["local"]["InstructionDigest"] for l in launches),
                   "Move changed trusted harness or approved instruction digest")
        return record

    def terminal(self, index):
        out = HOST.private_directory(self.out / ("terminal-%d" % index))
        self.run(["python3", str(Path(__file__).resolve()), "terminal", "--session-binary", str(self.args.session_binary.resolve()),
                  "--pty-helper", str(self.args.pty_helper.resolve()), "--socket", str(self.socket), "--session-id", self.id,
                  "--prompt-index", str(index), "--output-directory", str(out), "--timeout", str(self.args.turn_timeout)],
                 "terminal-%d" % index, timeout=self.args.turn_timeout + 30)
        evidence = json.loads((out / "terminal-evidence.json").read_text())
        BASE.check(evidence["passed"] and evidence["cliExitCode"] == 0, "Copilot attachment failed")
        conversations = [r for r in self.provider(quiet=True) if r["kind"] == "conversation"]
        expected = [(0, "answer"), (1, "tool-call"), (1, "answer"), (2, "answer")][:(1, 3, 4)[index]]
        BASE.check([(r["turn"], r["phase"]) for r in conversations] == expected, "Unexpected Responses sequence")
        if index >= 1:
            BASE.check(conversations[2].get("toolOutputValidated"), "Copilot's bash output was not received")
        self.report["steps"].append({"turn": index, "attachmentDetached": True, "nativeHistoryValidatedInRequest": True})
        self.save()
        return conversations

    def inspect_checkpoint(self, record, expected, count):
        checkpoint = record["checkpoint"]
        journal = (Path(self.config["directory"]) / "journal").resolve()
        path = (journal / checkpoint["path"]).resolve()
        BASE.check(path.is_relative_to(journal) and path.stat().st_size <= self.config["local"]["MaxCheckpointBytes"],
                   "Committed artifact escapes journal or exceeds bound")
        artifact = path.read_bytes()
        BASE.check(BASE.digest(artifact) == checkpoint["metadata"]["sha256"], "Trusted checkpoint digest differs")
        body = json.loads(artifact)
        BASE.check(body["version"] == 2 and body["harness"] == "copilot" and "codex" not in body and "claude" not in body,
                   "Portable checkpoint is not exclusively the Copilot adapter")
        native = body["copilot"]
        BASE.check(set(native) == {"ThreadID", "Events"} and native["ThreadID"] == record["threadId"],
                   "Portable native state is more than the selected journal")
        events = [json.loads(line) for line in base64.b64decode(native["Events"], validate=True).splitlines() if line]
        ends = [e["id"] for e in events if e["type"] == "assistant.turn_end" and not e.get("data", {}).get("parentToolCallId")]
        BASE.check(ends and ends[-1] == checkpoint["metadata"]["turnId"], "Checkpoint doesn't bind the journal's last turn")
        BASE.check(events[0]["type"] == "session.start" and events[0]["data"]["sessionId"] == record["threadId"],
                   "Journal is not the selected session")
        messages = [e["data"].get("content", "") for e in events if e["type"] == "assistant.message"]
        for answer in ANSWERS[:count]:
            BASE.check(answer in messages, "The journal lacks a prior answer")
        restored = self.out / ("restored-turn-%d" % count)
        self.run([str(self.checkpoint_binary.resolve()), "restore", "--destination", str(restored), "--max-bytes",
                  str(self.config["local"]["MaxCheckpointBytes"])], "restore-final", data=artifact)
        BASE.check(self.workspace(restored / "workspace") == expected, "Git workspace or bash effect changed on restore")
        files = [p.relative_to(restored / "copilot").as_posix() for p in (restored / "copilot").rglob("*") if p.is_file()]
        BASE.check(files == ["session-state/%s/events.jsonl" % record["threadId"]],
                   "Native restore included sign-in, settings or other session state")
        return {"sha256": BASE.digest(artifact), "bytes": len(artifact), "turnId": ends[-1], "journalEvents": len(events),
                "copilotVersion": events[0]["data"].get("copilotVersion"), "gitWorkspaceMatches": True,
                "portableNativeStateIsJournalOnly": True}

    def execute(self):
        BASE.check(not self.provider(), "Use a fresh fixture provider")
        markers = ["PROFILE_MARKER_DOCKER_FIRST", "PROFILE_MARKER_KUBERNETES", "PROFILE_MARKER_DOCKER_EDITED"]
        BASE.write_profile(self.fixture, markers[0])
        repo, expected = self.seed()
        start = self.cli("run", operation=uuid.uuid4(), workspace=repo, **{"include-ignored": ["needed.bin"]})
        BASE.check(start["initialStage"] == "active", "Launch inactive")
        deadline, thread = start["deadline"], None
        self.report["originalDeadline"] = deadline
        for index, destination in enumerate((self.args.remote_name, self.config["local"]["Name"], self.args.remote_name)):
            conversations = self.terminal(index)
            # Copilot reads its instructions at each launch, so each visit's
            # turn carries the profile as it was when that visit began.
            turn = [r for r in conversations if r["turn"] == index]
            BASE.check(turn and all(markers[index] in r["profileMarkers"] for r in turn),
                       "Turn %d didn't carry its visit's profile" % index)
            if index + 1 < len(markers):
                BASE.write_profile(self.fixture, markers[index + 1])
            if index == 1:
                expected["files"][TOOL_FILE] = BASE.digest((TOOL_MARKER + "\n").encode())
                expected["untracked"] = sorted([*expected["untracked"], TOOL_FILE])
            record = self.validate_record(self.cli("move", operation=uuid.uuid4(), to=destination), deadline, thread, destination)
            thread = record["threadId"]
            checkpoint = self.inspect_checkpoint(record, expected, index + 1)
            self.report["steps"].append({"move": destination, "epoch": record["epoch"], "checkpoint": checkpoint})
            self.report.update(threadId=thread, checkpoint=checkpoint)
            self.save()
        cancelled = self.validate_record(self.cli("cancel", operation=uuid.uuid4()), deadline, thread)
        BASE.check(cancelled["phase"] == "cancelled" and not cancelled.get("active"), "Cancellation did not stop authority")
        requests = self.provider(quiet=True)
        BASE.check(len([r for r in requests if r["kind"] == "conversation"]) == 4 and all(r["completed"] for r in requests),
                   "A move or cancel caused another conversation inference")
        used = sum(v["receipt"]["usage"]["model.inference"]["Units"] for v in cancelled["visits"])
        BASE.check(used == UNITS_PER_REQUEST * len(requests), "Sealed usage differs from the provider's usage")
        self.report.update(passed=True, finalPhase="cancelled", sameNativeUUID=True, sameOriginalDeadline=True,
                           actualBashToolAndFileVerified=True, stoppedVisits=len(cancelled["visits"]),
                           accounting={"sealedUnits": used, "providerRequests": len(requests)})
        self.save()


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    prep = commands.add_parser("prepare")
    prep.add_argument("--output-directory", type=Path, required=True)
    for name in ("docker-binary", "docker-socket"):
        prep.add_argument("--" + name, type=Path, required=True)
    for name in ("native-image-id", "helper-image-id", "anchor-image-id", "network-name", "gateway-ip"):
        prep.add_argument("--" + name, required=True)
    prep.add_argument("--units", type=int, default=200000)
    prep.add_argument("--duration", default="20m")
    prep.add_argument("--max-checkpoint-bytes", type=int, default=8 << 20)
    prep.set_defaults(gateway_port=18463, provider_port=PORT, provider_tls=True, instructions=INSTRUCTIONS)
    serving = commands.add_parser("serve")
    serving.add_argument("--output-directory", type=Path, required=True)
    serving.add_argument("--listen", default="0.0.0.0")
    serving.add_argument("--port", type=int, default=PORT)
    rendering = commands.add_parser("render")
    for name in ("control-image", "native-image"):
        rendering.add_argument("--" + name, required=True)
    for name in ("hints", "ca-file", "gateway-cert", "gateway-key", "profile-config-output"):
        rendering.add_argument("--" + name, type=Path, required=True)
    attaching = commands.add_parser("terminal")
    for name in ("session-binary", "pty-helper", "socket", "output-directory"):
        attaching.add_argument("--" + name, type=Path, required=True)
    attaching.add_argument("--session-id", required=True)
    attaching.add_argument("--prompt-index", type=int, choices=range(len(PROMPTS)), required=True)
    attaching.add_argument("--timeout", type=int, default=120)
    verifying = commands.add_parser("verify")
    for name in ("output-directory", "home-config", "session-binary", "pty-helper", "checkpoint-binary"):
        verifying.add_argument("--" + name, type=Path, required=True)
    verifying.add_argument("--remote-name", default="remote")
    verifying.add_argument("--context")
    verifying.add_argument("--operation-timeout", type=int, default=240)
    verifying.add_argument("--turn-timeout", type=int, default=120)
    args = parser.parse_args()
    if args.command == "verify":
        proof = Proof(args)
        try:
            proof.execute()
        except Exception as error:
            proof.report["failure"] = {"type": type(error).__name__, "message": str(error)}
            proof.save()
            raise
        print(json.dumps(proof.report))
    else:
        {"prepare": prepare, "serve": serve, "render": render, "terminal": terminal}[args.command](args)


if __name__ == "__main__":
    main()
