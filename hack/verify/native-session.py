#!/usr/bin/env python3
"""Exercise production checkpoint capture/restore with real stock Codex resumes.

The loopback provider makes no paid requests. This gate proves short text history
across isolated native homes; Docker/Kind, tool state and fencing have separate
acceptance gates. --change-instructions is an expected-failure diagnostic: the
pinned exec resume path retains the original developer instructions.
"""
import argparse
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import threading
import time


OUT = Path(__file__).resolve().parents[2] / ".local/verification/native-session"
REQUESTS = []
RESPONSES = ["NATIVE_FIRST_COMPLETE", "NATIVE_SECOND_COMPLETE", "NATIVE_THIRD_COMPLETE"]
PROMPTS = ["Remember the first fixture fact: amber-otter-481.",
           "Continue the conversation. Remember the second fixture fact: cobalt-wren-729.",
           "Continue using both facts already in this conversation."]
DEVELOPERS = ["Destination developer instruction: INITIAL_APPROVED_111.",
              "Destination developer instruction: REMOTE_APPROVED_222.",
              "Destination developer instruction: RETURN_APPROVED_333."]
CHANGE_INSTRUCTIONS = False


class Provider(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        index = len(REQUESTS)
        REQUESTS.append({"path": self.path, "body": body,
                         "port": self.server.server_port,
                         "placeholderAuth": self.headers.get("Authorization") == "Bearer fixture-placeholder"})
        (OUT / "requests.json").write_text(json.dumps(REQUESTS, indent=2) + "\n")
        if index >= len(RESPONSES):
            self.send_error(400, "Extra unexpected request")
            return
        encoded = json.dumps(body)
        required = [PROMPTS[index], DEVELOPERS[index if CHANGE_INSTRUCTIONS else 0]]
        if index >= 1:
            required += ["amber-otter-481", RESPONSES[0]]
        if index >= 2:
            required += ["cobalt-wren-729", RESPONSES[1]]
        absent = [value for value in required if value not in encoded]
        REQUESTS[-1]["missingRequiredHistory"] = absent
        (OUT / "requests.json").write_text(json.dumps(REQUESTS, indent=2) + "\n")
        if absent:
            self.send_error(400, "History or current instructions missing")
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        answer = RESPONSES[index]
        response = {"id": "resp_native_" + str(index), "object": "response",
                    "created_at": int(time.time()), "status": "in_progress",
                    "model": body["model"], "output": []}
        item = {"id": "msg_native_" + str(index), "type": "message", "status": "in_progress",
                "role": "assistant", "content": []}
        part = {"type": "output_text", "text": "", "annotations": []}
        def event(kind, **fields):
            data = {"type": kind, **fields}
            self.wfile.write(("data: " + json.dumps(data) + "\n\n").encode())
            self.wfile.flush()
        event("response.created", response=response)
        event("response.output_item.added", output_index=0, item=item)
        event("response.content_part.added", item_id=item["id"], output_index=0, content_index=0, part=part)
        event("response.output_text.delta", item_id=item["id"], output_index=0, content_index=0, delta=answer)
        event("response.output_text.done", item_id=item["id"], output_index=0, content_index=0, text=answer)
        part["text"] = answer
        item.update(status="completed", content=[part])
        event("response.content_part.done", item_id=item["id"], output_index=0, content_index=0, part=part)
        event("response.output_item.done", output_index=0, item=item)
        response.update(status="completed", output=[item], usage={"input_tokens": 10, "output_tokens": 5, "total_tokens": 15})
        event("response.completed", response=response)


def census(path):
    return [{"path": str(p.relative_to(path)), "bytes": p.stat().st_size,
             "sha256": hashlib.sha256(p.read_bytes()).hexdigest()}
            for p in sorted(path.rglob("*")) if p.is_file()]


def main():
    global CHANGE_INSTRUCTIONS, OUT
    parser = argparse.ArgumentParser()
    parser.add_argument("--state", choices=["rollout"], default="rollout",
                        help="use production capture/restore of the exact native thread")
    parser.add_argument("--change-instructions", action="store_true",
                        help="negative diagnostic; expects native resume to retain old instructions")
    parser.add_argument("--output-directory", type=Path, default=OUT)
    args = parser.parse_args()
    CHANGE_INSTRUCTIONS = args.change_instructions
    OUT = args.output_directory.resolve()
    OUT.mkdir(parents=True, exist_ok=True)
    temp_parent = "/private/tmp" if Path("/private/tmp").is_dir() else None
    base = Path(tempfile.mkdtemp(prefix="sproozi-native-proof-", dir=temp_parent)).resolve()
    checkpoint_binary = base / "native-checkpoint"
    build_command = ["go", "build", "-o", str(checkpoint_binary), "./hack/verify/native-checkpoint"]
    build = subprocess.run(build_command, cwd=Path(__file__).resolve().parents[2],
                           capture_output=True, text=True, timeout=120)
    (OUT / "checkpoint-build.stdout.log").write_text(build.stdout)
    (OUT / "checkpoint-build.stderr.log").write_text(build.stderr)
    assert build.returncode == 0, "Production checkpoint helper build failed: " + build.stderr
    version_home = base / "version-home"
    version_home.mkdir()
    (version_home / ".codex").mkdir()
    version_env = {"PATH": os.environ["PATH"], "HOME": str(version_home), "CODEX_HOME": str(version_home / ".codex")}
    version = subprocess.check_output(["codex", "--version"], env=version_env, text=True).strip()
    assert version == "codex-cli 0.161.0", "Unsupported native version: " + version
    servers = [ThreadingHTTPServer(("127.0.0.1", 0), Provider) for _ in range(3)]
    for server in servers:
        threading.Thread(target=server.serve_forever, daemon=True).start()
    evidence = {"nativeVersion": version,
                "temporaryRoot": str(base), "stateSelection": args.state,
                "instructionMode": "changed" if CHANGE_INSTRUCTIONS else "invariant",
                "expectedFailureDiagnostic": CHANGE_INSTRUCTIONS,
                "checkpointAdapter": "internal/harness.CaptureCodexState/RestoreCodexState/CompletedCodexTurn",
                "checkpointBuildCommand": build_command, "turns": [],
                "sourceDigests": {str(p.relative_to(Path(__file__).resolve().parents[2])): hashlib.sha256(p.read_bytes()).hexdigest()
                                  for p in [Path(__file__).resolve(),
                                            Path(__file__).resolve().parents[2] / "internal/harness/codex_state.go",
                                            Path(__file__).resolve().parents[2] / "hack/verify/native-checkpoint/main.go"]},
                "limitations": ["Three isolated homes on one host; no Docker, Kubernetes, CNI or production authority proof",
                                "Short text-only history; long pagination, compaction and tool/subprocess state unassessed",
                                "Invariant trusted instruction digest required; exec resume does not replace prior developer instructions",
                                "Deterministic provider validates transported history; no paid inference or reasoning claim"]}
    thread_id = None
    prior_home = None
    try:
        for index in range(3):
            location = base / ("location-" + str(index))
            home = location / "home"
            native = home / ".codex"
            workspace = location / ("workspace-" + str(index))
            native.mkdir(parents=True)
            workspace.mkdir(parents=True)
            copied = []
            restored = None
            if prior_home:
                restored = json.loads(subprocess.check_output(
                    [str(checkpoint_binary), "--source-home", str(prior_home),
                     "--destination-home", str(native), "--thread", thread_id], text=True, timeout=15))
                copied = [restored["path"]]
                assert restored["threadId"] == thread_id and restored["restored"], restored
                restored_files = census(native)
                assert len(restored_files) == 1, restored_files
                assert restored_files[0]["sha256"] == restored["sha256"], restored_files
            env = {"PATH": os.environ["PATH"], "HOME": str(home), "CODEX_HOME": str(native),
                   "TMPDIR": str(location), "XDG_CONFIG_HOME": str(home / "config"),
                   "XDG_DATA_HOME": str(home / "data"), "XDG_CACHE_HOME": str(home / "cache"),
                   "XDG_STATE_HOME": str(home / "state"), "FIXTURE_MODEL_KEY_" + str(index): "fixture-placeholder"}
            config = {"model_provider": "native-fixture", "model_providers.native-fixture.name": "Fixture",
                      "model_providers.native-fixture.base_url": "http://127.0.0.1:" + str(servers[index].server_port) + "/v1",
                      "model_providers.native-fixture.env_key": "FIXTURE_MODEL_KEY_" + str(index),
                      "model_providers.native-fixture.wire_api": "responses",
                      "model_providers.native-fixture.supports_websockets": False,
                      "developer_instructions": DEVELOPERS[index if CHANGE_INSTRUCTIONS else 0]}
            cmd = ["codex", "exec", "--ignore-user-config", "--ignore-rules", "--json",
                   "--skip-git-repo-check", "--model", "gpt-6.1-sol"]
            for key, value in config.items():
                cmd += ["-c", key + "=" + json.dumps(value)]
            if thread_id:
                cmd += ["resume", thread_id, PROMPTS[index]]
            else:
                cmd += ["--sandbox", "read-only", PROMPTS[index]]
            result = subprocess.run(cmd, cwd=workspace, env=env, capture_output=True, text=True, timeout=45)
            (OUT / ("turn-" + str(index) + ".stdout.jsonl")).write_text(result.stdout)
            (OUT / ("turn-" + str(index) + ".stderr.log")).write_text(result.stderr)
            events = [json.loads(line) for line in result.stdout.splitlines() if line.strip()]
            ids = [event["thread_id"] for event in events if event.get("type") == "thread.started"]
            if not thread_id and ids:
                thread_id = ids[0]
            facts = {"index": index, "command": cmd, "cwd": str(workspace), "exitCode": result.returncode,
                     "threadIds": ids, "copiedTopLevel": copied, "files": census(native),
                     "responseObserved": any(event.get("type") == "item.completed" and
                         event.get("item", {}).get("text") == RESPONSES[index] for event in events)}
            evidence["turns"].append(facts)
            facts["restoreMetadata"] = restored
            rollouts = list((native / "sessions").rglob("*" + thread_id + ".jsonl"))
            if len(rollouts) == 1:
                records = [json.loads(line) for line in rollouts[0].read_text().splitlines()]
                facts["nativeEventSequence"] = [r.get("payload", {}).get("type") for r in records if r.get("type") == "event_msg"]
                facts["nativeTail"] = records[-3:]
                shutil.copy2(rollouts[0], OUT / ("turn-" + str(index) + ".rollout.jsonl"))
            (OUT / "evidence.json").write_text(json.dumps(evidence, indent=2) + "\n")
            assert result.returncode == 0, result.stderr
            assert ids == [thread_id], ids
            assert facts["responseObserved"], result.stdout
            assert REQUESTS[index]["missingRequiredHistory"] == [], REQUESTS[index]
            assert REQUESTS[index]["placeholderAuth"], REQUESTS[index]
            assert REQUESTS[index]["port"] == servers[index].server_port, REQUESTS[index]
            assert any(event.get("type") == "turn.completed" for event in events), events
            assert facts["nativeEventSequence"][-1] == "task_complete", facts["nativeEventSequence"]
            facts["captureMetadata"] = json.loads(subprocess.check_output(
                [str(checkpoint_binary), "--source-home", str(native), "--thread", thread_id],
                text=True, timeout=15))
            assert facts["captureMetadata"]["threadId"] == thread_id, facts["captureMetadata"]
            (OUT / "evidence.json").write_text(json.dumps(evidence, indent=2) + "\n")
            prior_home = native
            print("PASS", index, "thread=" + thread_id, "copied=" + str(copied), "files=" + str(len(facts["files"])), flush=True)
        evidence["passed"] = True
        evidence["requests"] = REQUESTS
    except BaseException as exc:
        evidence["passed"] = False
        evidence["error"] = str(exc)
        if CHANGE_INSTRUCTIONS:
            evidence["expectedInstructionOverrideRejected"] = (
                len(REQUESTS) == 2 and DEVELOPERS[1] in REQUESTS[1].get("missingRequiredHistory", []))
            if evidence["expectedInstructionOverrideRejected"]:
                print("EXPECTED FAILURE: native exec resume retained its original developer instructions", flush=True)
        raise
    finally:
        (OUT / "evidence.json").write_text(json.dumps(evidence, indent=2) + "\n")
        for server in servers:
            server.shutdown()


if __name__ == "__main__":
    main()
