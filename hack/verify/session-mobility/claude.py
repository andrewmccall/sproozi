#!/usr/bin/env python3
"""Credential-free Claude mobility fixture and public-CLI proof.

prepare/serve/render are administrator helpers; verify uses only the production
session socket and checkpoint CLI. TLS keys, journals and terminal diagnostics
remain in owned private directories. A completed provider reply is not a native
Stop hook or a writer fence: only production Stop supplies those observations.
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
import re
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
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


HOST = module("claude_mobility_host", HERE / "host.py")
BASE = module("claude_mobility_base", HERE / "verify.py")
KUBE = module("claude_mobility_kube", HERE / "kubernetes.py")
PROBES = module("claude_mobility_probes", HERE / "admin_probes.py")
TOKEN = "claude-fixture-placeholder"
MODEL = "claude-sonnet-4-5"
INSTRUCTIONS = "Only this deterministic local fixture is approved. INITIAL_CLAUDE_111."
TOOL_MARKER = "TOOL_FACT=ochre-mole-392"
TOOL_FILE = "claude-tool-effect.txt"
TOOL_COMMAND = "printf 'TOOL_FACT=ochre-mole-392\\n' > claude-tool-effect.txt; cat claude-tool-effect.txt"
PROMPTS = ["Remember the fixture fact silver-tern-681.",
           "Use Bash to print TOOL_FACT=ochre-mole-392, then remember that fact.",
           "Continue using both facts from this conversation.",
           "Complete the controlled held Claude reply using the first fixture fact."]
ANSWERS = ["CLAUDE_FIRST_COMPLETE", "CLAUDE_TOOL_COMPLETE", "CLAUDE_RESUMED_COMPLETE", "CLAUDE_HELD_COMPLETE"]
SYSTEM, AGENTS = "sproozi-system", "sproozi-agents"
GATEWAY = "sproozi-gateway." + SYSTEM + ".svc"
TEMPLATE = "mobility-claude"
UPSTREAM = "https://192.168.65.254:18775"


def prepare(args):
    out = HOST.private_directory(args.output_directory)
    BASE.check(not any(out.iterdir()), "Prepare requires an empty private directory")
    # Reuse host-only Docker/config/certificate plumbing, then explicitly select
    # the Claude adapter. No Codex options enter the published Claude config.
    with contextlib.redirect_stdout(io.StringIO()):
        HOST.prepare(args)
    names = ["DNS:api.anthropic.com", "DNS:api.openai.com", "DNS:" + GATEWAY,
             "DNS:" + GATEWAY + ".cluster.local", "IP:127.0.0.1", "IP:" + args.gateway_ip]
    HOST.write(out / "leaf.ext", "basicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\n"
               "extendedKeyUsage=serverAuth\nsubjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid,issuer\n"
               "subjectAltName=" + ",".join(names) + "\n")
    HOST.run(["openssl", "x509", "-req", "-sha256", "-in", "gateway.csr", "-CA", "ca.pem", "-CAkey", "ca.key",
              "-CAserial", "ca.srl", "-CAcreateserial", "-out", "gateway.crt", "-days", "2", "-extfile", "leaf.ext"], out)
    for hostname in ("api.anthropic.com", GATEWAY, "127.0.0.1", args.gateway_ip):
        option = "-verify_ip" if hostname in ("127.0.0.1", args.gateway_ip) else "-verify_hostname"
        HOST.run(["openssl", "verify", "-x509_strict", "-CAfile", "ca.pem", "-purpose", "sslserver", option,
                  hostname, "gateway.crt"], out)
    HOST.write(out / "provider-token", TOKEN + "\n")
    HOST.write(out / "profile-token", os.urandom(32).hex() + "\n")
    config = json.loads((out / "home-config.json").read_text())
    config["model"], config["local"]["Harness"] = MODEL, "claude-code"
    config.pop("nativeOptions", None)
    config["remote"] = {args.remote_name: {"url": "https://127.0.0.1:18454",
        "tokenFile": str(out / "profile-token"), "caFile": str(out / "ca.pem"),
        "maxCheckpointBytes": args.max_checkpoint_bytes}}
    HOST.write(out / "home-config.json", config)
    hints = json.loads((out / "profile-hints.json").read_text())
    hints.update(harness="claude-code", model=MODEL, nativeOptions=[],
                 providerToken=TOKEN, providerURLFromKind=UPSTREAM, prompts=PROMPTS[:3], answers=ANSWERS[:3],
                 heldPrompt=PROMPTS[3], heldAnswer=ANSWERS[3],
                 limitations=["Deterministic Messages fixture, not live Anthropic service acceptance",
                              "Provider completion alone is not native completion or a writer fence"])
    hints.pop("extendedPrompts", None)
    hints.pop("extendedAnswers", None)
    HOST.write(out / "profile-hints.json", hints)
    print(json.dumps({"homeConfig": str(out / "home-config.json"), "hints": str(out / "profile-hints.json"),
                      "harness": "claude-code", "providerTokenIsPublicPlaceholder": True}))


def classify(body):
    messages = body.get("messages", [])
    system = "\n".join(c.get("text", "") for c in body.get("system", []) if isinstance(c, dict))
    # The native TUI issues these auxiliary Haiku calls independently of
    # the main conversation. They carry no approved instructions or history;
    # classify them explicitly rather than pretending they are resumed turns.
    if body.get("model") == "claude-haiku-4-5-20251001":
        if "Analyze if this message indicates a new conversation topic." in system:
            kind, answer = "title", '{"isNewTopic":false,"title":null}'
        elif "Your task is to process Bash commands that an AI coding agent wants to run." in system:
            kind, answer = "bash-prefix", "none"
        elif "Extract any file paths that this command reads or modifies." in system:
            kind, answer = "file-paths", "<filepaths>\nclaude-tool-effect.txt\n</filepaths>"
        else:
            raise ValueError("Unexpected auxiliary Claude request")
        return {"kind": kind, "turn": None, "phase": "answer", "missingHistory": [], "fixtureAnswer": answer}
    if body.get("model") != MODEL:
        raise ValueError("Unexpected conversation model")
    users = [m.get("content") for m in messages if m.get("role") == "user" and
             not (isinstance(m.get("content"), list) and any(c.get("type") == "tool_result"
                 for c in m["content"] if isinstance(c, dict)))]
    text = json.dumps(users[-1]) if users else ""
    matches = [i for i, prompt in enumerate(PROMPTS) if prompt in text]
    if len(matches) != 1:
        raise ValueError("Unexpected Claude conversation request")
    turn = matches[0]
    encoded = json.dumps(body)
    required = [INSTRUCTIONS, PROMPTS[turn]]
    if turn >= 1:
        required += [PROMPTS[0], ANSWERS[0]]
    if turn >= 2:
        required += [PROMPTS[1], ANSWERS[1], TOOL_MARKER]
    missing = [item for item in required if item not in encoded]
    results = [c for m in messages if isinstance(m.get("content"), list)
               for c in m["content"] if isinstance(c, dict) and c.get("type") == "tool_result"]
    result = {"kind": "conversation", "turn": turn, "phase": "answer", "missingHistory": missing}
    if turn == 1 and not results:
        if not any(t.get("name") == "Bash" for t in body.get("tools", [])):
            raise ValueError("Actual Claude Bash tool was not advertised")
        result["phase"] = "tool-call"
    elif turn == 1:
        tool_output = json.dumps(results)
        if TOOL_MARKER not in tool_output or any(c.get("is_error") for c in results):
            raise ValueError("Actual Bash result did not contain the fixture fact")
        result.update(toolOutputValidated=True, toolOutputSHA256=hashlib.sha256(tool_output.encode()).hexdigest())
    return result


def reply(writer, body, request, index):
    tool = request["phase"] == "tool-call"
    block = ({"type": "tool_use", "id": "toolu_claude_mobility", "name": "Bash",
              "input": {"command": TOOL_COMMAND, "description": "Write and read deterministic fixture fact"}}
             if tool else {"type": "text", "text": request.get("fixtureAnswer") or ANSWERS[request["turn"]]})
    message = {"id": "msg_claude_mobility_" + str(index), "type": "message", "role": "assistant",
               "model": body["model"], "content": [block], "stop_reason": "tool_use" if tool else "end_turn",
               "stop_sequence": None, "usage": {"input_tokens": 32, "output_tokens": 8}}
    if not body.get("stream"):
        writer.wfile.write(json.dumps(message).encode())
        writer.wfile.flush()
        return
    def event(kind, **fields):
        writer.wfile.write(("event: " + kind + "\ndata: " + json.dumps({"type": kind, **fields}) + "\n\n").encode())
        writer.wfile.flush()
    event("message_start", message={**message, "content": [], "stop_reason": None,
                                   "usage": {"input_tokens": 32, "output_tokens": 0}})
    event("content_block_start", index=0, content_block={**block, **({"input": {}} if tool else {"text": ""})})
    event("content_block_delta", index=0, delta=({"type": "input_json_delta", "partial_json": json.dumps(block["input"])}
          if tool else {"type": "text_delta", "text": block["text"]}))
    event("content_block_stop", index=0)
    event("message_delta", delta={"stop_reason": message["stop_reason"], "stop_sequence": None}, usage={"output_tokens": 8})
    event("message_stop")


def provider_class(args, out, stop, records, lock):
    def persist():
        HOST.write(out / "requests.private.json", records)
    class Provider(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_GET(self):
            if self.path != "/status":
                self.send_error(404)
                return
            fields = ("kind", "turn", "phase", "completed", "missingHistory", "placeholderAuth", "usage", "held",
                      "holdID", "released", "streamError", "toolOutputSHA256", "toolOutputValidated", "profileMarkers",
                      "profileSkill")
            with lock:
                data = json.dumps({"requests": [{k: r[k] for k in fields if k in r} for r in records]}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(data)

        def do_POST(self):
            try:
                size = int(self.headers.get("Content-Length", "0"))
                if not 0 < size <= 1 << 20:
                    self.send_error(413)
                    return
                body = json.loads(self.rfile.read(size))
                if self.headers.get("x-api-key") != TOKEN:
                    self.send_error(403, "Only the public fixture credential is accepted")
                    return
                path = urlsplit(self.path).path
                if path == "/v1/messages/count_tokens":
                    content = b'{"input_tokens": 32}'
                    self.send_response(200)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(content)))
                    self.end_headers()
                    self.wfile.write(content)
                    return
                if path != "/v1/messages":
                    self.send_error(404)
                    return
                request = classify(body)
                request.update(body=body, placeholderAuth=True, completed=False,
                               profileMarkers=BASE.profile_markers(json.dumps(body)),
                               profileSkill=("profile:" + BASE.PROFILE_SKILL) in json.dumps(body))
                with lock:
                    if len(records) >= 128:
                        self.send_error(429)
                        return
                    index = len(records)
                    records.append(request)
                    persist()
                if request["missingHistory"]:
                    self.send_error(400, "Required actual native history absent")
                    return
                self.send_response(200)
                self.send_header("Content-Type", "text/event-stream" if body.get("stream") else "application/json")
                self.send_header("Connection", "close")
                self.end_headers()
                hold_id = HOST.matching_hold(out, request) if request["kind"] == "conversation" else None
                if hold_id:
                    with lock:
                        request.update(held=True, holdID=hold_id, released=False)
                        persist()
                    HOST.await_release(out, hold_id, args.hold_timeout, stop)
                    with lock:
                        request["released"] = True
                        persist()
                reply(self, body, request, index)
                with lock:
                    request.update(completed=True, usage={"input_tokens": 32, "output_tokens": 8})
                    persist()
            except (ValueError, KeyError, TypeError) as error:
                self.send_error(400, type(error).__name__)
            except (BrokenPipeError, ConnectionResetError, RuntimeError, TimeoutError) as error:
                with lock:
                    if "request" in locals():
                        request["streamError"] = type(error).__name__
                    persist()
    return Provider


def serve(args):
    out = HOST.private_directory(args.output_directory)
    stop, lock, records = threading.Event(), threading.RLock(), []
    server = ThreadingHTTPServer((args.listen, args.port), provider_class(args, out, stop, records, lock))
    tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    tls.minimum_version = ssl.TLSVersion.TLSv1_3
    tls.load_cert_chain(out / "gateway.crt", out / "gateway.key")
    server.socket = tls.wrap_socket(server.socket, server_side=True)
    server.daemon_threads = True
    HOST.write(out / "requests.private.json", records)
    HOST.write(out / "provider-listener.json", {"listen": args.listen, "port": server.server_port,
               "tls": True, "fixtureTokenIsPublicPlaceholder": True, "prompts": PROMPTS, "answers": ANSWERS})
    def shutdown(*_):
        stop.set()
        threading.Thread(target=server.shutdown, daemon=True).start()
    signal.signal(signal.SIGTERM, shutdown)
    signal.signal(signal.SIGINT, shutdown)
    try:
        server.serve_forever()
    finally:
        stop.set()
        server.server_close()


def render(args):
    import yaml
    BASE.check(args.hints.stat().st_size <= 64 << 10, "Profile hints exceed fixture bound")
    hints = json.loads(args.hints.read_text())
    BASE.check(hints["harness"] == "claude-code" and
               hints["providerToken"] == TOKEN and hints["providerURLFromKind"] == UPSTREAM and
               hints["nativeOptions"] == [] and hints["model"] == MODEL, "Only the configured Claude fixture is permitted")
    # Existing validator checks all shared bounds and exact instruction digest;
    # these three local substitutions select its infrastructure-only contract.
    validation = {**hints, "providerToken": HOST.TOKEN, "providerURLFromKind": KUBE.UPSTREAM}
    scratch = args.profile_config_output.parent / ("validate-" + uuid.uuid4().hex + ".json")
    HOST.write(scratch, validation)
    try:
        KUBE.hints_from(scratch)
    finally:
        scratch.unlink()
    BASE.check(KUBE.IMAGE.fullmatch(args.control_image) and KUBE.IMAGE.fullmatch(args.native_image),
               "Both images must be canonical repository@sha256 references")
    ca = args.ca_file.read_text()
    BASE.check(len(ca.encode()) <= 1 << 20 and re.fullmatch(
        r"(?:\s*-----BEGIN CERTIFICATE-----\s+[A-Za-z0-9+/=\s]+-----END CERTIFICATE-----\s*)+", ca),
        "CA must contain only bounded public PEM certificates")
    ssl.create_default_context(cadata=ca)
    trust = KUBE.trust_name(ca, "claude-" + KUBE.TRUST)
    # The dedicated proof cluster already has one controller/RBAC/CNI. Never
    # introduce a second controller watching the same ordinary AgentRuns.
    documents = [d for d in KUBE.build("config/default") if d and d["kind"] == "Deployment" and
                 d["metadata"]["name"] == "sproozi-gateway"]
    for doc in documents:
        if doc["kind"] == "Deployment":
            KUBE.configure_deployment(doc, args.control_image, trust)
        if doc["kind"] == "Deployment" and doc["metadata"]["name"] == "sproozi-gateway":
            container = doc["spec"]["template"]["spec"]["containers"][0]
            container["env"] += [{"name": "ANTHROPIC_UPSTREAM_URL", "value": UPSTREAM},
                {"name": "ANTHROPIC_API_KEY", "valueFrom": {"secretKeyRef": {
                    "name": "claude-mobility-model-placeholder", "key": "anthropic-api-key"}}}]
    resources = KUBE.fixture_resources(hints, args.native_image, ca, trust)
    for doc in resources:
        if doc["kind"] == "AgentRuntime":
            doc["spec"]["clientConfig"]["harness"] = "claude-code"
        if doc["kind"] == "Secret":
            doc["metadata"]["name"] = "claude-mobility-model-placeholder"
            doc["stringData"] = {"anthropic-api-key": TOKEN}
    documents.extend(resources)
    replacements = {KUBE.TEMPLATE: TEMPLATE}
    def substitute(value):
        if isinstance(value, list):
            return [substitute(v) for v in value]
        if isinstance(value, dict):
            return {k: substitute(v) for k, v in value.items()}
        if isinstance(value, str):
            for before, after in replacements.items():
                value = value.replace(before, after)
        return value
    documents = substitute(documents)
    profile = {"listen": "127.0.0.1:18454", "certificateFile": str(args.gateway_cert.resolve(strict=True)),
               "keyFile": str(args.gateway_key.resolve(strict=True)), "tokenFile": str(args.profile_config_output.parent / "profile-token"),
               "principal": "claude-mobility-proof", "harness": "claude-code", "placementName": "remote",
               "namespace": SYSTEM, "templateName": TEMPLATE, "directory": str(args.profile_config_output.parent / "remote-profile-state"),
               "instructionDigest": hints["instructionDigest"], "allowances": hints["allowances"],
               "maxDuration": hints["maxDuration"], "maxCheckpointBytes": hints["maxCheckpointBytes"]}
    HOST.private_directory(args.profile_config_output.parent)
    BASE.check(not args.profile_config_output.is_symlink(), "Profile output cannot be a symlink")
    output = yaml.safe_dump_all(documents, sort_keys=False)
    HOST.write(args.profile_config_output, profile)
    sys.stdout.write(output)


def terminal(args):
    out = HOST.private_directory(args.output_directory)
    tui = HOST.TUI
    tui.OUT, tui.HELPER = out, args.pty_helper.resolve()
    home = HOST.private_directory(out / "cli-home")
    env = {"PATH": os.environ["PATH"], "HOME": str(home), "TERM": "xterm-256color",
           "COLORTERM": "truecolor", "LANG": "en_US.UTF-8"}
    command = [str(args.session_binary.resolve()), "attach", "--socket", str(args.socket), "--id", args.session_id]
    evidence = {"passed": False, "harness": "claude-code", "promptIndex": args.prompt_index,
                "path": "actual production sproozi attach and stock Claude TUI", "command": command}
    supervisor = tui.Supervisor(command, out, env, "attachment", out)
    try:
        connection = supervisor.connect()
        BASE.check(supervisor.wait_text("Claude", args.timeout), "Stock Claude TUI did not render through attach")
        time.sleep(0.5)
        tui.submit(connection, PROMPTS[args.prompt_index])
        BASE.check(supervisor.wait_text(ANSWERS[args.prompt_index], args.timeout), "Actual Claude answer was not rendered")
        supervisor.command(b"\x1d")
        end = time.monotonic() + 10
        while time.monotonic() < end and not supervisor.rpc("status").get("exited"):
            time.sleep(0.05)
        BASE.check(supervisor.rpc("status").get("exited") and supervisor.wait(1) == 0, "Attachment did not detach cleanly")
        evidence.update(passed=True, cliExitCode=supervisor.returncode, terminalProbes=supervisor.probes,
                        attachmentMetrics=supervisor.metrics, terminalBytes=len(supervisor.data),
                        limitation="Answer text is diagnostic; production Stop independently checks native hooks and writer termination")
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
        BASE.check(self.config["local"]["Harness"] == "claude-code" and self.config["model"] == MODEL and
                   not self.config.get("nativeOptions") and hints["prompts"] == PROMPTS[:3] and hints["answers"] == ANSWERS[:3] and
                   Path(self.config["gateway"]["providerTokenFile"]).read_text().strip() == TOKEN,
                   "Only the configured public Claude fixture configuration is allowed")
        self.socket = Path(self.config["directory"]) / "control.sock"
        self.id = str(uuid.uuid4())
        self.env = {"PATH": os.environ["PATH"], "HOME": str(self.out), "LANG": "C.UTF-8",
                    "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull, "GIT_TERMINAL_PROMPT": "0"}
        self.report = {"passed": False, "harness": "claude-code", "sessionId": self.id,
                       "fixtureCredentialIsPublicPlaceholder": True, "steps": [],
                       "scope": "stock Claude TUI and actual Bash through production session placements",
                       "limitations": ["Deterministic Messages provider; no paid Anthropic request",
                           "Compaction and hostile transcripts are unassessed",
                           "Containment controls require separate administrator evidence"]}
        self.save()
        self.probes = PROBES.NativeDenials(self) if args.admin_probes else None
        if self.probes:
            self.report["limitations"][-1] = "Administrator probes cover the fixed worker denial cases; hostile administrators unassessed"
            self.report["administratorProbes"] = True
            self.save()

    def workspace(self, repo):
        result = super().workspace(repo)
        if (repo / TOOL_FILE).exists():
            result["files"][TOOL_FILE] = BASE.digest((repo / TOOL_FILE).read_bytes())
        return result

    def validate_record(self, status, deadline, thread=None, placement=None):
        record = super().validate_record(status, deadline, thread, placement)
        launches = [v["launch"] for v in record["visits"]]
        if record.get("active"):
            launches.append(record["active"]["launch"])
        BASE.check(record["harness"] == "claude-code" and all(l["harness"] == "claude-code" and
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
        BASE.check(evidence["passed"] and evidence["cliExitCode"] == 0, "Actual Claude attachment failed")
        requests = self.provider(quiet=True)
        expected = [(0, "answer"), (1, "tool-call"), (1, "answer"), (2, "answer")][:(1, 3, 4)[index]]
        conversations = [r for r in requests if r["kind"] == "conversation"]
        BASE.check([(r["turn"], r["phase"]) for r in conversations] == expected, "Unexpected actual Messages sequence")
        if index >= 1:
            BASE.check(conversations[2]["toolOutputValidated"], "Actual Bash output was not received")
        self.report["steps"].append({"turn": index, "attachmentDetached": True, "providerRequests": len(requests),
                                     "nativeHistoryValidatedInRequest": True})
        self.save()

    def inspect_checkpoint(self, record, expected, count):
        checkpoint = record["checkpoint"]
        journal = (Path(self.config["directory"]) / "journal").resolve()
        path = (journal / checkpoint["path"]).resolve()
        BASE.check(path.is_relative_to(journal) and path.stat().st_size <= self.config["local"]["MaxCheckpointBytes"],
                   "Committed artifact escapes journal or exceeds bound")
        artifact = path.read_bytes()
        BASE.check(BASE.digest(artifact) == checkpoint["metadata"]["sha256"], "Trusted checkpoint digest differs")
        body = json.loads(artifact)
        BASE.check(body["version"] == 2 and body["harness"] == "claude-code" and "codex" not in body,
                   "Portable checkpoint is not exclusively the selected Claude adapter")
        native = body["claude"]
        BASE.check(set(native) == {"ThreadID", "Workspace", "Path", "Transcript"},
                   "Portable native state contains configuration beyond the selected transcript")
        BASE.check(native["ThreadID"] == record["threadId"] and checkpoint["metadata"]["harness"] == "claude-code",
                   "Checkpoint changed selected native UUID or harness")
        transcript = base64.b64decode(native["Transcript"], validate=True)
        events = [json.loads(line) for line in transcript.splitlines() if line]
        records = [e for e in events if e.get("type") in ("user", "assistant") and not e.get("isSidechain")]
        BASE.check(records and records[-1]["uuid"] == checkpoint["metadata"]["turnId"],
                   "Trusted stopped checkpoint does not bind actual native transcript leaf")
        BASE.check(all(e.get("sessionId") == record["threadId"] and bool(e.get("version")) for e in records),
                   "Transcript is not the genuine selected conversation")
        for answer in ANSWERS[:count]:
            BASE.check(any(block.get("type") == "text" and block.get("text") == answer
                          for e in records if e["type"] == "assistant" for block in e["message"].get("content", [])),
                       "Genuine native history lacks the prior answer")
        restored = self.out / ("restored-turn-%d" % count)
        self.run([str(self.checkpoint_binary.resolve()), "restore", "--destination", str(restored), "--max-bytes",
                  str(self.config["local"]["MaxCheckpointBytes"])], "restore-final", data=artifact)
        actual = self.workspace(restored / "workspace")
        BASE.check(actual == expected, "Git history, index, unfinished files or actual Bash effect changed on restore")
        BASE.check(not (restored / "workspace/.env").exists() and not (restored / "workspace/ignored").exists(),
                   "Unselected ignored files were transferred")
        portable_files = [p.relative_to(restored / "claude").as_posix()
                          for p in (restored / "claude").rglob("*") if p.is_file()]
        BASE.check(len(portable_files) == 1 and portable_files[0].endswith("/" + record["threadId"] + ".jsonl"),
                   "Native restore included credentials, settings or unrelated session state")
        return {"sha256": BASE.digest(artifact), "bytes": len(artifact), "nativeTranscriptSHA256": BASE.digest(transcript),
                "stoppedNativeLeaf": records[-1]["uuid"], "nativeVersion": records[-1]["version"], "gitWorkspaceMatches": True,
                "actualBashFilePreserved": count >= 2, "ignoredFilesExcluded": True, "selectedIgnoredFilePreserved": True,
                "portableNativeStateIsTranscriptOnly": True,
                "completionEvidence": "production placement Stop hook, actual writer fence and immutable receipt; leaf is compatibility evidence"}

    def execute(self):
        BASE.check(not self.provider(), "Use a fresh fixture provider")
        # Each visit's launch must carry the profile as it was when that visit
        # began: Docker reads the live folder, Kubernetes the archived copy.
        markers = ["PROFILE_MARKER_DOCKER_FIRST", "PROFILE_MARKER_KUBERNETES", "PROFILE_MARKER_DOCKER_EDITED",
                   "PROFILE_MARKER_KUBERNETES_FINAL"]
        BASE.write_profile(self.fixture, markers[0])
        repo, expected = self.seed()
        start = self.cli("start", operation=uuid.uuid4(), workspace=repo, **{"include-ignored": ["needed.bin"]})
        BASE.check(start["initialStage"] == "active" and not start.get("threadId"), "Initial native identity fabricated or launch inactive")
        deadline, thread = start["deadline"], None
        self.report["originalDeadline"] = deadline
        self.save()
        for index, destination in enumerate((self.args.remote_name, self.config["local"]["Name"], self.args.remote_name)):
            self.terminal(index)
            # Claude Code resumes a conversation with the system prompt and skill
            # listing it started with, so model requests keep the first visit's
            # profile; each visit's own launch must still render its profile.
            turns = [r for r in self.provider(quiet=True) if r["kind"] == "conversation" and r["turn"] == index]
            BASE.check(turns and all(r["profileMarkers"] == [markers[0]] and r["profileSkill"] for r in turns),
                       "Turn %d model requests lost the conversation's profile" % index)
            step = {"turn": index, "requestProfileMarker": markers[0], "profileSkill": True}
            if self.probes:
                active = self.cli("status")["session"]["active"]
                rendered = self.probes.read(active, "/tmp/sproozi-native/profile-plugin/skills/" + BASE.PROFILE_SKILL + "/SKILL.md")
                BASE.check(BASE.profile_markers(rendered) == [markers[index]],
                           "Visit %d did not render the profile as it was at launch" % index)
                step.update(renderedProfileMarker=markers[index], placement=active["launch"]["placement"])
            self.report["steps"].append(step)
            BASE.write_profile(self.fixture, markers[index + 1])
            if self.probes and index < 2:
                active = None if index == 0 else self.cli("status")["session"]["active"]
                self.probes.run(active)
            if index == 1:
                expected["files"][TOOL_FILE] = BASE.digest((TOOL_MARKER + "\n").encode())
                expected["untracked"] = sorted([*expected["untracked"], TOOL_FILE])
            record = self.validate_record(self.cli("move", operation=uuid.uuid4(), to=destination), deadline, thread, destination)
            thread = record["threadId"]
            BASE.check(record["harness"] == "claude-code", "Logical session changed native adapter")
            checkpoint = self.inspect_checkpoint(record, expected, index + 1)
            self.report["steps"].append({"move": destination, "epoch": record["epoch"], "checkpoint": checkpoint})
            self.report.update(threadId=thread, checkpoint=checkpoint)
            self.save()
        cancelled = self.validate_record(self.cli("cancel", operation=uuid.uuid4()), deadline, thread)
        BASE.check(cancelled["phase"] == "cancelled" and not cancelled.get("active"), "Final cancellation did not stop authority")
        requests = self.provider(quiet=True)
        BASE.check(len([r for r in requests if r["kind"] == "conversation"]) == 4 and all(r["completed"] for r in requests),
                   "Move or cancel caused another conversation inference")
        used = sum(v["receipt"]["usage"]["model.inference"]["Units"] for v in cancelled["visits"])
        BASE.check(used == 40 * len(requests), "Trusted sealed usage differs from actual Messages SSE usage")
        BASE.check(all(v["receipt"]["usage"]["model.inference"]["CostMicros"] == 0 for v in cancelled["visits"]),
                   "Unexpected fixture cost usage")
        self.report.update(passed=True, finalPhase="cancelled", sameNativeUUID=True, sameOriginalDeadline=True,
                           actualBashToolAndFileVerified=True, stoppedVisits=len(cancelled["visits"]),
                           accounting={"sealedUnits": used, "providerRequests": len(requests), "tokensPerRequest": 40,
                                       "remainingUnits": cancelled["remaining"]["model.inference"]["units"]["value"]})
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
    prep.set_defaults(gateway_port=18453, provider_port=18775, provider_tls=True, remote_name="remote",
                      remote_url=None, instructions=INSTRUCTIONS, remote_token_file=None, remote_ca_file=None)
    serving = commands.add_parser("serve")
    serving.add_argument("--output-directory", type=Path, required=True)
    serving.add_argument("--listen", default="0.0.0.0")
    serving.add_argument("--port", type=int, default=18775)
    serving.add_argument("--hold-timeout", type=float, default=180)
    controlling = commands.add_parser("control")
    controlling.add_argument("--output-directory", type=Path, required=True)
    controlling.add_argument("--operation", choices=["arm", "release"], required=True)
    controlling.add_argument("--prompt-index", type=int, choices=range(len(PROMPTS)), default=3)
    controlling.add_argument("--reply-phase", choices=["answer", "tool-call"], default="answer")
    controlling.add_argument("--hold-id")
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
    attaching.add_argument("--timeout", type=int, default=90)
    verifying = commands.add_parser("verify")
    for name in ("output-directory", "home-config", "session-binary", "pty-helper", "checkpoint-binary"):
        verifying.add_argument("--" + name, type=Path, required=True)
    verifying.add_argument("--remote-name", default="remote")
    verifying.add_argument("--operation-timeout", type=int, default=240)
    verifying.add_argument("--turn-timeout", type=int, default=90)
    verifying.add_argument("--admin-probes", action="store_true", help="Run fixed denials in exact admitted containers using administrator access")
    verifying.add_argument("--kubeconfig", type=Path, help="Administrator-only dedicated mobility Kind configuration")
    verifying.add_argument("--kubectl", type=Path, default=Path("kubectl"))
    verifying.add_argument("--namespace", default=SYSTEM)
    verifying.add_argument("--agent-namespace", default=AGENTS)
    args = parser.parse_args()
    if args.command == "verify":
        if bool(args.kubeconfig) != args.admin_probes:
            parser.error("Use --admin-probes together with --kubeconfig, or omit both")
        proof = Proof(args)
        try:
            proof.execute()
        except Exception as error:
            proof.report["failure"] = {"type": type(error).__name__, "message": str(error)}
            proof.save()
            raise
        print(json.dumps(proof.report))
    else:
        {"prepare": prepare, "serve": serve, "control": HOST.control, "render": render, "terminal": terminal}[args.command](args)


if __name__ == "__main__":
    main()
