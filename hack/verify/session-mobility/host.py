#!/usr/bin/env python3
"""Private fixture support for the production session daemon's Docker/Kind proof.

Reuses native-tui.py's deterministic Responses protocol and genuine Go PTY client.
This helper neither starts placements nor supplies Kubernetes credentials. The
root verifier owns the actual start/move/cancel/recover and containment assertions.
All model credentials here are public placeholder bytes; no paid provider calls.
"""
import argparse
import hashlib
import importlib.util
import json
import os
import re
from pathlib import Path
import signal
import shlex
import ssl
import subprocess
import threading
import time
import uuid
from http.server import ThreadingHTTPServer

ROOT = Path(__file__).resolve().parents[3]
spec = importlib.util.spec_from_file_location("sproozi_native_tui_fixture", ROOT / "hack/verify/native-tui.py")
TUI = importlib.util.module_from_spec(spec)
spec.loader.exec_module(TUI)
INSTRUCTIONS = "Only the deterministic mobility fixture is approved. TUI_INITIAL_APPROVED_111"
TOOL_MARKER = "NATIVE_TOOL_EFFECT_fern_badger_451"
TOOL_FILE = "native-tool-effect.txt"
TOOL_PROMPT = "Run the controlled fixture exec_command to write native-tool-effect.txt with NATIVE_TOOL_EFFECT_fern_badger_451 and read it back."
HOLD_PROMPT = "Complete the controlled held mobility reply using the first fixture fact."
HOMEOPS_PROMPT = "Inspect the approved home-ops fixture Pod through kubernetes.read, attempt the prohibited Secrets read, and report the observed state and denial."
HOMEOPS_MARKER = "HOMEOPS_CAPABILITIES_VERIFIED"
PROMPTS = [TUI.FIRST, TUI.SECOND, TUI.THIRD]
BUILD_PROMPT = "Run the approved offline Sproozi package tests and report their actual result; retain this native conversation for local continuation."
BUILD_MARKER = "SPROOZI_PACKAGE_TESTS_VERIFIED"
ALL_PROMPTS = [*PROMPTS, TOOL_PROMPT, HOLD_PROMPT, HOMEOPS_PROMPT, BUILD_PROMPT]
ANSWERS = [*TUI.ANSWERS, "TUI_TOOL_COMPLETE", "TUI_HELD_COMPLETE", "HOMEOPS_TASK_COMPLETE", "SPROOZI_BUILD_COMPLETE"]
TOKEN = "tui-placeholder"


def private_directory(path):
    path = path.resolve()
    path.mkdir(parents=True, exist_ok=True, mode=0o700)
    stat = path.lstat()
    if not path.is_dir() or path.is_symlink() or stat.st_mode & 0o077 or stat.st_uid != os.geteuid():
        raise RuntimeError("Fixture output must be a private owned directory")
    return path


def write(path, content):
    if isinstance(content, (dict, list)):
        content = json.dumps(content, indent=2) + "\n"
    path.write_text(content)
    path.chmod(0o600)


def run(args, directory):
    subprocess.run(args, cwd=directory, env={"PATH": os.environ["PATH"], "HOME": str(directory)},
                   check=True, capture_output=True, timeout=30)


def prepare(args):
    out = private_directory(args.output_directory)
    instructions = args.instructions
    digest = hashlib.sha256(instructions.encode()).hexdigest()
    ca, ca_key = out / "ca.pem", out / "ca.key"
    leaf, key = out / "gateway.crt", out / "gateway.key"
    if any(path.exists() for path in [ca, ca_key, leaf, key]):
        raise RuntimeError("Use a fresh output directory; retained trust keys cannot be replaced")
    write(out / "leaf.ext", "subjectAltName=DNS:api.openai.com,DNS:kubernetes.default.svc,DNS:sproozi-work.sproozi-system.svc,DNS:sproozi-gateway.sproozi-system.svc,DNS:sproozi-gateway.sproozi-system.svc.cluster.local,IP:127.0.0.1,IP:" + args.gateway_ip +
          "\nbasicConstraints=critical,CA:FALSE\nkeyUsage=critical,digitalSignature,keyEncipherment\n"
          "extendedKeyUsage=serverAuth\nsubjectKeyIdentifier=hash\nauthorityKeyIdentifier=keyid,issuer\n")
    run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-sha256", "-days", "2",
         "-addext", "basicConstraints=critical,CA:TRUE", "-addext", "keyUsage=critical,keyCertSign,cRLSign",
         "-addext", "subjectKeyIdentifier=hash",
         "-subj", "/CN=Sproozi mobility fixture CA", "-keyout", str(ca_key), "-out", str(ca)], out)
    run(["openssl", "req", "-new", "-newkey", "rsa:2048", "-nodes", "-subj", "/CN=api.openai.com",
         "-keyout", str(key), "-out", str(out / "gateway.csr")], out)
    run(["openssl", "x509", "-req", "-sha256", "-days", "2", "-in", str(out / "gateway.csr"),
         "-CA", str(ca), "-CAkey", str(ca_key), "-CAcreateserial", "-extfile", str(out / "leaf.ext"),
         "-out", str(leaf)], out)
    for host in ["127.0.0.1", args.gateway_ip]:
        run(["openssl", "verify", "-x509_strict", "-CAfile", str(ca), "-verify_ip", host, str(leaf)], out)
    run(["openssl", "verify", "-x509_strict", "-CAfile", str(ca), "-verify_hostname", "api.openai.com",
         str(leaf)], out)
    for path in out.iterdir():
        if path.is_file():
            path.chmod(0o600)
    write(out / "provider-token", TOKEN + "\n")
    allowances = {"model.inference": {"units": {"bounded": True, "value": args.units},
                                       "costMicros": {"bounded": False, "value": 0}}}
    options = ["model_provider=\"mobility-fixture\"",
               "model_providers.mobility-fixture.name=\"Mobility fixture\"",
               "model_providers.mobility-fixture.base_url=\"https://api.openai.com/v1\"",
               "model_providers.mobility-fixture.env_key=\"OPENAI_API_KEY\"",
               "model_providers.mobility-fixture.wire_api=\"responses\"",
               "model_providers.mobility-fixture.supports_websockets=false",
               "projects.\"/workspace/session/workspace\".trust_level=\"trusted\""]
    remote = {}
    if args.remote_url:
        if not args.remote_token_file or not args.remote_ca_file:
            raise RuntimeError("Fixed remote URL requires its token and CA files")
        remote[args.remote_name] = {"URL": args.remote_url, "TokenFile": str(args.remote_token_file.resolve()),
                                    "CAFile": str(args.remote_ca_file.resolve()), "MaxCheckpointBytes": args.max_checkpoint_bytes}
    cfg = {"directory": str(out / "home"), "dockerBinary": str(args.docker_binary.resolve()),
           "dockerSocket": str(args.docker_socket.absolute()), "dockerConfig": str(out / "docker-cli"),
           "local": {"Harness": "codex", "Name": "local", "NativeImageID": args.native_image_id, "HelperImageID": args.helper_image_id,
                     "AnchorImageID": args.anchor_image_id, "NetworkName": args.network_name,
                     "GatewayIPv4": args.gateway_ip, "GatewayPort": args.gateway_port,
                     "Instructions": instructions, "InstructionDigest": digest,
                     "AgentLimits": {"MemoryBytes": 1073741824, "CPUMilli": 1000, "PIDs": 256},
                     "HelperLimits": {"MemoryBytes": 536870912, "CPUMilli": 500, "PIDs": 128},
                     "AnchorLimits": {"MemoryBytes": 67108864, "CPUMilli": 100, "PIDs": 32},
                     "VolumeBytes": 536870912, "MaxCheckpointBytes": args.max_checkpoint_bytes,
                     "RetainedBytes": 128 << 20},
           "allowances": allowances, "maxDuration": args.duration, "model": "gpt-6.1-sol",
           "nativeOptions": options,
           "gateway": {"listen": "0.0.0.0:" + str(args.gateway_port), "certificateFile": str(leaf),
                       "keyFile": str(key), "trustBundleFile": str(ca),
                       "providerURL": ("https" if args.provider_tls else "http") + "://127.0.0.1:" + str(args.provider_port),
                       "providerTokenFile": str(out / "provider-token"),
                       "mcpAuthority": "sproozi-gateway.sproozi-system.svc:443"}, "remote": remote}
    if args.provider_tls:
        cfg["gateway"]["providerCAFile"] = str(ca)
    write(out / "home-config.json", cfg)
    write(out / "profile-hints.json", {"instructions": instructions, "instructionDigest": digest,
          "allowances": allowances, "nativeOptions": options,
          "providerToken": TOKEN, "providerURLFromKind": ("https" if args.provider_tls else "http") + "://" + args.gateway_ip + ":" + str(args.provider_port),
          "providerCAFile": str(ca) if args.provider_tls else None,
          "strictCertificateChainValidated": True,
          "maxCheckpointBytes": args.max_checkpoint_bytes, "maxDuration": args.duration,
          "prompts": PROMPTS, "answers": TUI.ANSWERS,
          "extendedPrompts": ALL_PROMPTS, "extendedAnswers": ANSWERS,
          "limitations": ["Fixture HTTP provider binds the host for trusted gateway access; worker anchor must block its port",
                          "Short text-only native history; tool effects/compaction/GUI ownership are not proven"]})
    print(json.dumps({"config": str(out / "home-config.json"), "socket": str(out / "home/control.sock"),
                      "profileHints": str(out / "profile-hints.json")}))


def successful_exec_output(value, marker=TOOL_MARKER):
    # functions.exec wraps text() results in native content blocks. Decode the
    # nested JSON at each layer instead of depending on an escaping depth.
    if isinstance(value, str):
        try:
            return successful_exec_output(json.loads(value), marker)
        except (ValueError, TypeError):
            return False
    if isinstance(value, list):
        return any(successful_exec_output(item, marker) for item in value)
    if isinstance(value, dict):
        if value.get("exit_code") == 0 and marker in str(value.get("output", "")):
            return True
        return any(successful_exec_output(item, marker) for item in value.values())
    return False


def advertised(body, name):
    namespaces = body.get("tools", []) + [tool for item in body.get("input", [])
        if item.get("type") == "additional_tools" for tool in item.get("tools", [])]
    return any(tool.get("type") == "namespace" and tool.get("name") == "functions" and
               any(child.get("name") == name for child in tool.get("tools", [])) for tool in namespaces)


def classify_request(body):
    """Prompt identity survives title requests, tool followups and retries."""
    items = body.get("input", [])
    users = ["".join(c.get("text", "") for c in item.get("content", []))
             for item in items if item.get("role") == "user"]
    last = users[-1] if users else ""
    if last.startswith("Generate a concise, single-line task title"):
        return {"kind": "title", "turn": None, "phase": "answer", "missingHistory": []}
    turn = {prompt: index for index, prompt in enumerate(ALL_PROMPTS)}.get(last)
    if turn is None:
        raise ValueError("Unexpected conversation prompt")
    encoded = json.dumps(body)
    required = [[TUI.FIRST], [TUI.FIRST, ANSWERS[0], TUI.SECOND],
                [TUI.FIRST, ANSWERS[0], TUI.SECOND, ANSWERS[1], TUI.THIRD],
                [TUI.FIRST, ANSWERS[0], TOOL_PROMPT], [TUI.FIRST, ANSWERS[0], HOLD_PROMPT], [HOMEOPS_PROMPT], [BUILD_PROMPT]][turn]
    if turn in (5, 6) and users.count(ALL_PROMPTS[turn]) > 1:
        required.append(ANSWERS[turn])
    result = {"kind": "conversation", "turn": turn, "phase": "answer",
              "missingHistory": [text for text in required if text not in encoded]}
    if turn not in (3, 5, 6):
        return result
    # A completed earlier tool turn in resumed history is not this turn's result.
    last_user_index = max(i for i, item in enumerate(items) if item.get("role") == "user")
    current = items[last_user_index + 1:]
    outputs = [item for item in current if item.get("type") in ("custom_tool_call_output", "function_call_output")
               and (item.get("call_id") == "mobility_exec_tool" or item.get("call_id", "").startswith("mobility_wait_"))]
    if outputs:
        output = json.dumps(outputs)
        result.update(kind="tool-followup", toolOutputSHA256=hashlib.sha256(output.encode()).hexdigest(),
                      toolOutputValidated=successful_exec_output(outputs, HOMEOPS_MARKER if turn == 5 else BUILD_MARKER if turn == 6 else TOOL_MARKER))
        if not result["toolOutputValidated"]:
            running = re.search(r"Script running with cell ID ([a-zA-Z0-9_-]+)", str(outputs[-1].get("output", "")))
            if turn == 6 and running:
                if not advertised(body, "wait"):
                    raise ValueError("Native functions.wait was not advertised")
                result.update(phase="wait-call", waitCell=running.group(1))
            else:
                raise ValueError("Actual exec_command output is absent or failed")
    else:
        tools = body.get("tools", []) + [tool for item in items if item.get("type") == "additional_tools"
                                        for tool in item.get("tools", [])]
        supported = any(tool.get("type") == "namespace" and tool.get("name") == "functions" and
                        any(child.get("type") == "custom" and child.get("name") == "exec" and
                            "exec_command" in child.get("description", "") for child in tool.get("tools", []))
                        for tool in tools)
        if not supported:
            raise ValueError("Native functions.exec / exec_command was not advertised")
        result["phase"] = "tool-call"
    return result


def control(args):
    """Only the trusted host can arm/release; workers have no control HTTP route."""
    out = private_directory(args.output_directory)
    path = out / "hold.json"
    if args.operation == "arm":
        record = {"id": uuid.uuid4().hex, "turn": args.prompt_index, "phase": args.reply_phase,
                  "released": False}
    else:
        record = json.loads(path.read_text())
        if record["id"] != args.hold_id:
            raise RuntimeError("Release must name the exact armed hold ID")
        record["released"] = True
    temporary = out / ("hold-" + uuid.uuid4().hex + ".tmp")
    write(temporary, record)
    temporary.replace(path)
    print(json.dumps(record))


def matching_hold(out, request):
    path = out / "hold.json"
    if not path.exists():
        return None
    record = json.loads(path.read_text())
    if not record["released"] and record["turn"] == request["turn"] and record["phase"] == request["phase"]:
        return record["id"]
    return None


def await_release(out, hold_id, timeout, stop):
    deadline = time.monotonic() + timeout
    while not stop.is_set():
        record = json.loads((out / "hold.json").read_text())
        if record["id"] != hold_id:
            raise RuntimeError("Armed hold replaced while a real request was waiting")
        if record["released"]:
            return
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError("Held provider reply was not explicitly released")
        # Wait for host file change without manufacturing SSE or native completion.
        stop.wait(min(0.05, remaining))
    raise RuntimeError("Fixture provider stopped while reply remained held")


def response_output(writer, response, request, index):
    if request["phase"] == "wait-call":
        item = {"id": "fc_mobility_wait_" + str(index), "type": "function_call",
                "call_id": "mobility_wait_" + str(index), "status": "completed", "namespace": "functions",
                "name": "wait", "arguments": json.dumps(dict(cell_id=request["waitCell"], yield_time_ms=10000, max_tokens=1000))}
        TUI.event(writer, "response.output_item.added", output_index=0, item=item)
        TUI.event(writer, "response.output_item.done", output_index=0, item=item)
    elif request["phase"] == "tool-call":
        command = "printf '%s\\n' '" + TOOL_MARKER + "' > " + TOOL_FILE + "; cat " + TOOL_FILE
        if request["turn"] == 5:
            command = "node -e " + shlex.quote((ROOT / "hack/verify/portable-workflows/home-ops.js").read_text())
        if request["turn"] == 6:
            command = "go test -p 1 ./internal/project ./internal/workspace ./internal/work && printf 'SPROOZI_PACKAGE_TESTS_VERIFIED\\n'"
        arguments = {"cmd": command, "workdir": "/workspace/session/workspace", "login": False,
                     "max_output_tokens": 1000, "yield_time_ms": 1000}
        code = "text(await tools.exec_command(" + json.dumps(arguments) + "));"
        if request["turn"] == 6:
            code = "let result = await tools.exec_command(" + json.dumps(arguments) + "); while (result.session_id && result.exit_code == null) { result = await tools.write_stdin({session_id: result.session_id, chars: '', yield_time_ms: 1000, max_output_tokens: 1000}); } text(result);"
        item = {"id": "ctc_mobility_" + str(index), "type": "custom_tool_call",
                "call_id": "mobility_exec_tool", "status": "completed", "namespace": "functions",
                "name": "exec", "input": code}
        TUI.event(writer, "response.output_item.added", output_index=0, item=item)
        TUI.event(writer, "response.output_item.done", output_index=0, item=item)
    else:
        answer = "Remember TUI facts" if request["kind"] == "title" else ANSWERS[request["turn"]]
        if request["kind"] == "title" and request["body"].get("text", {}).get("format", {}).get("type") == "json_schema":
            answer = json.dumps({"title": answer})
        item = {"id": "msg_mobility_" + str(index), "type": "message", "status": "in_progress",
                "role": "assistant", "content": []}
        part = {"type": "output_text", "text": "", "annotations": []}
        TUI.event(writer, "response.output_item.added", output_index=0, item=item)
        TUI.event(writer, "response.content_part.added", item_id=item["id"], output_index=0, content_index=0, part=part)
        TUI.event(writer, "response.output_text.delta", item_id=item["id"], output_index=0, content_index=0, delta=answer)
        TUI.event(writer, "response.output_text.done", item_id=item["id"], output_index=0, content_index=0, text=answer)
        part["text"] = answer
        item.update(status="completed", content=[part])
        TUI.event(writer, "response.content_part.done", item_id=item["id"], output_index=0, content_index=0, part=part)
        TUI.event(writer, "response.output_item.done", output_index=0, item=item)
    usage = {"input_tokens": 10, "output_tokens": 5, "total_tokens": 15}
    response.update(status="completed", output=[item], usage=usage)
    TUI.event(writer, "response.completed", response=response)
    return usage


def provider_class(args, out, stop):
    class Provider(TUI.Provider):
        def do_POST(self):
            size = int(self.headers.get("Content-Length", "0"))
            if size <= 0 or size > 1 << 20:
                self.send_error(413, "Fixture body exceeds bound")
                return
            body = json.loads(self.rfile.read(size))
            try:
                request = classify_request(body)
            except ValueError as error:
                self.send_error(400, str(error))
                return
            developer_text = [content.get("text", "") for item in body.get("input", [])
                              if item.get("role") == "developer" for content in item.get("content", [])]
            approved = any(args.instructions in text for text in developer_text)
            authenticated = self.headers.get("Authorization") == "Bearer " + TOKEN
            if not authenticated or request["kind"] != "title" and not approved:
                TUI.EVIDENCE["unexpectedProviderAuthority"] = True
                TUI.persist()
                self.send_error(403, "Fixture authority or approved instructions absent")
                return
            request.update(path=self.path, body=body, placeholderAuth=authenticated, completed=False)
            with TUI.LOCK:
                index = len(TUI.REQUESTS)
                TUI.REQUESTS.append(request)
                TUI.persist()
            if request["missingHistory"]:
                self.send_error(400, "Required conversation history absent")
                return
            self.send_response(200)
            self.send_header("Content-Type", "text/event-stream")
            self.end_headers()
            response = {"id": "resp_mobility_" + str(index), "object": "response",
                        "created_at": int(time.time()), "status": "in_progress",
                        "model": body["model"], "output": []}
            try:
                TUI.event(self, "response.created", response=response)
                hold_id = matching_hold(out, request)
                if hold_id:
                    with TUI.LOCK:
                        request.update(held=True, holdID=hold_id, released=False)
                        TUI.persist()
                    await_release(out, hold_id, args.hold_timeout, stop)
                    with TUI.LOCK:
                        request["released"] = True
                        TUI.persist()
                usage = response_output(self, response, request, index)
                with TUI.LOCK:
                    request.update(completed=True, usage=usage)
                    TUI.persist()
            except (BrokenPipeError, ConnectionResetError, RuntimeError, TimeoutError) as error:
                # A cancelled turn has no delivered completed response or actual
                # usage claim. The gateway's conservative reservation remains.
                with TUI.LOCK:
                    request["streamError"] = type(error).__name__
                    TUI.persist()

        def do_GET(self):
            if self.path != "/status":
                self.send_error(404)
                return
            fields = ["kind", "turn", "phase", "completed", "missingHistory", "placeholderAuth",
                      "usage", "held", "holdID", "released", "streamError", "toolOutputSHA256",
                      "toolOutputValidated"]
            with TUI.LOCK:
                content = json.dumps({"requests": [{key: r[key] for key in fields if key in r}
                                                   for r in TUI.REQUESTS]}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(content)))
            self.end_headers()
            self.wfile.write(content)
    return Provider


def provider(args):
    out = private_directory(args.output_directory)
    TUI.OUT, TUI.REQUESTS = out, []
    TUI.EVIDENCE = {"fixture": "production session mobility", "approvedInstructions": args.instructions,
                    "providerTokenIsPublicPlaceholder": True, "historyValidatedByNativeRequests": True,
                    "completionMeaning": "response.completed flushed to upstream socket; gateway delivery and native task_complete require separate evidence"}
    stop = threading.Event()
    server = ThreadingHTTPServer((args.listen, args.port), provider_class(args, out, stop))
    if args.tls:
        tls = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
        tls.minimum_version = ssl.TLSVersion.TLSv1_2
        tls.load_cert_chain(args.certificate_file or out / "gateway.crt", args.key_file or out / "gateway.key")
        server.socket = tls.wrap_socket(server.socket, server_side=True)
    server.daemon_threads = True
    TUI.persist()
    write(out / "provider-listener.json", {"listen": args.listen, "port": server.server_port, "tls": args.tls,
                                           "fixtureToken": TOKEN, "prompts": ALL_PROMPTS, "answers": ANSWERS})
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
        TUI.persist()


def terminal(args):
    out = private_directory(args.output_directory)
    TUI.OUT, TUI.HELPER = out, args.pty_helper.resolve()
    home = private_directory(out / "cli-home")
    env = {"PATH": os.environ["PATH"], "HOME": str(home), "TERM": "xterm-256color",
           "COLORTERM": "truecolor", "LANG": "en_US.UTF-8"}
    command = [str(args.session_binary.resolve()), "attach", "--socket", str(args.socket), "--id", args.session_id]
    evidence = {"command": command, "promptIndex": args.prompt_index,
                "path": "actual cmd/sproozi byte terminal through production Go PTY", "passed": False}
    write(out / "terminal-evidence.json", evidence)
    supervisor = TUI.Supervisor(command, out, env, "attachment", out)
    try:
        connection = supervisor.connect()
        if not supervisor.wait_text("Codex", args.timeout):
            raise RuntimeError("Actual native TUI did not render through sproozi attach")
        time.sleep(0.5)
        rendered = TUI.clean_terminal(bytes(supervisor.data))
        if "Trustthisfolder?" in rendered or "Trust this folder?" in rendered:
            # This is the controlled Git fixture restored at the fixed runtime
            # path. Complete its stock-client trust prompt before typing a turn.
            supervisor.command(b"\r")
            if not supervisor.wait_text("Ask Codex to do anything", args.timeout):
                raise RuntimeError("Native folder trust did not reach the conversation prompt")
            time.sleep(0.5)
        TUI.submit(connection, ALL_PROMPTS[args.prompt_index])
        if not supervisor.wait_text(ANSWERS[args.prompt_index], args.timeout):
            raise RuntimeError("Native conversation turn did not complete in terminal output")
        supervisor.command(b"\x1d")
        end = time.monotonic() + 10
        while time.monotonic() < end and not supervisor.rpc("status").get("exited"):
            time.sleep(0.05)
        if not supervisor.rpc("status").get("exited") or supervisor.wait(1) != 0:
            raise RuntimeError("sproozi attach did not detach successfully")
        evidence.update(passed=True, cliExitCode=supervisor.returncode, terminalProbes=supervisor.probes,
                        attachmentMetrics=supervisor.metrics,
                        limitation="Terminal output is not the completed-turn/writer fence; placement Stop must independently confirm it")
    finally:
        evidence["terminalBytes"] = len(supervisor.data)
        write(out / "terminal-evidence.json", evidence)
        supervisor.close()
    print(json.dumps(evidence))


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="action", required=True)
    prep = commands.add_parser("prepare")
    prep.add_argument("--output-directory", type=Path, required=True)
    prep.add_argument("--docker-binary", type=Path, required=True)
    prep.add_argument("--docker-socket", type=Path, required=True)
    for name in ["native-image-id", "helper-image-id", "anchor-image-id", "network-name", "gateway-ip"]:
        prep.add_argument("--" + name, required=True)
    prep.add_argument("--gateway-port", type=int, default=18443)
    prep.add_argument("--provider-port", type=int, default=18765)
    prep.add_argument("--provider-tls", action="store_true")
    prep.add_argument("--remote-name", default="remote")
    prep.add_argument("--remote-url")
    prep.add_argument("--remote-token-file", type=Path)
    prep.add_argument("--remote-ca-file", type=Path)
    prep.add_argument("--instructions", default=INSTRUCTIONS)
    prep.add_argument("--units", type=int, default=200000)
    prep.add_argument("--duration", default="15m")
    prep.add_argument("--max-checkpoint-bytes", type=int, default=8 << 20)
    serving = commands.add_parser("provider")
    serving.add_argument("--output-directory", type=Path, required=True)
    serving.add_argument("--listen", default="0.0.0.0")
    serving.add_argument("--port", type=int, default=18765)
    serving.add_argument("--instructions", default=INSTRUCTIONS)
    serving.add_argument("--hold-timeout", type=float, default=180)
    serving.add_argument("--tls", action="store_true")
    serving.add_argument("--certificate-file", type=Path)
    serving.add_argument("--key-file", type=Path)
    attaching = commands.add_parser("terminal")
    for name in ["session-binary", "pty-helper", "socket", "output-directory"]:
        attaching.add_argument("--" + name, type=Path, required=True)
    attaching.add_argument("--session-id", required=True)
    attaching.add_argument("--prompt-index", type=int, choices=range(len(ALL_PROMPTS)), required=True)
    attaching.add_argument("--timeout", type=int, default=45)
    controlling = commands.add_parser("control")
    controlling.add_argument("--output-directory", type=Path, required=True)
    controlling.add_argument("--operation", choices=["arm", "release"], required=True)
    controlling.add_argument("--prompt-index", type=int, choices=range(len(ALL_PROMPTS)), default=4)
    controlling.add_argument("--reply-phase", choices=["answer", "tool-call"], default="answer")
    controlling.add_argument("--hold-id")
    args = parser.parse_args()
    {"prepare": prepare, "provider": provider, "terminal": terminal, "control": control}[args.action](args)


if __name__ == "__main__":
    main()
