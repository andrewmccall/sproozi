#!/usr/bin/env python3
"""Offline checks of the observed Claude fixture protocol; no listener or model."""
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import threading
import types
import unittest

spec = importlib.util.spec_from_file_location("claude_fixture", Path(__file__).with_name("claude.py"))
C = importlib.util.module_from_spec(spec)
spec.loader.exec_module(C)


def request(turn, tool_result=None):
    messages = []
    for index in range(turn + 1):
        messages.append({"role": "user", "content": C.PROMPTS[index]})
        if index < turn:
            messages.append({"role": "assistant", "content": [{"type": "text", "text": C.ANSWERS[index]}]})
    if turn >= 2:
        messages.insert(3, {"role": "user", "content": [{"type": "tool_result",
            "tool_use_id": "toolu_claude_mobility", "content": "TOOL_FACT=ochre-mole-392\n"}]})
    if tool_result is not None:
        messages.append({"role": "user", "content": [{"type": "tool_result",
            "tool_use_id": "toolu_claude_mobility", "content": tool_result}]})
    return {"model": "claude-sonnet-4-5", "system": [{"type": "text", "text": C.INSTRUCTIONS}],
            "messages": messages, "tools": [{"name": "Bash"}], "stream": True}


class FixtureTests(unittest.TestCase):
    def post(self, path, body, token=C.TOKEN):
        with tempfile.TemporaryDirectory() as directory:
            records = []
            handler_class = C.provider_class(types.SimpleNamespace(hold_timeout=1), Path(directory),
                                             threading.Event(), records, threading.RLock())
            handler = handler_class.__new__(handler_class)
            encoded = json.dumps(body).encode()
            handler.path, handler.rfile, handler.wfile = path, io.BytesIO(encoded), io.BytesIO()
            handler.headers = {"Content-Length": str(len(encoded)), "x-api-key": token}
            statuses, headers = [], {}
            handler.send_response = statuses.append
            handler.send_error = lambda code, *_: statuses.append(code)
            handler.send_header = lambda key, value: headers.update({key: value})
            handler.end_headers = lambda: None
            handler.do_POST()
            return statuses, headers, handler.wfile.getvalue(), records

    def test_beta_query_and_bounded_count_token_response(self):
        statuses, headers, content, records = self.post("/v1/messages/count_tokens?beta=true", request(0))
        self.assertEqual(statuses, [200])
        self.assertEqual(int(headers["Content-Length"]), len(content))
        self.assertEqual(json.loads(content), {"input_tokens": 32})
        self.assertEqual(records, [])
        statuses, headers, content, records = self.post("/v1/messages?beta=true", request(0))
        self.assertEqual(statuses, [200])
        self.assertEqual(headers["Content-Type"], "text/event-stream")
        self.assertIn(b'"text": "CLAUDE_FIRST_COMPLETE"', content)
        self.assertTrue(records[0]["completed"])
        self.assertEqual(records[0]["usage"], {"input_tokens": 32, "output_tokens": 8})

    def test_unknown_credential_does_not_enter_accepted_accounting(self):
        statuses, _, content, records = self.post("/v1/messages?beta=true", request(0), "unapproved-fixture-key")
        self.assertEqual(statuses, [403])
        self.assertEqual(content, b"")
        self.assertEqual(records, [])

    def test_rejects_missing_instructions_and_previous_answers(self):
        body = request(2)
        self.assertEqual(C.classify(body)["missingHistory"], [])
        body["system"] = []
        self.assertIn(C.INSTRUCTIONS, C.classify(body)["missingHistory"])
        body["system"] = [{"type": "text", "text": C.INSTRUCTIONS}]
        body["messages"] = [m for m in body["messages"] if m["role"] != "assistant"]
        self.assertIn("CLAUDE_FIRST_COMPLETE", C.classify(body)["missingHistory"])
        self.assertIn("CLAUDE_TOOL_COMPLETE", C.classify(body)["missingHistory"])

    def test_bash_requires_advertised_tool_and_actual_result(self):
        body = request(1)
        self.assertEqual(C.classify(body)["phase"], "tool-call")
        body["tools"] = []
        with self.assertRaisesRegex(ValueError, "not advertised"):
            C.classify(body)
        with self.assertRaisesRegex(ValueError, "Bash result"):
            C.classify(request(1, "The tool did not run"))
        actual = C.classify(request(1, "TOOL_FACT=ochre-mole-392\n"))
        self.assertEqual(actual["phase"], "answer")
        self.assertTrue(actual["toolOutputValidated"])

    def test_actual_anthropic_stream_shape_and_workspace_command(self):
        writer = types.SimpleNamespace(wfile=io.BytesIO())
        C.reply(writer, request(1), C.classify(request(1)), 1)
        frames = [json.loads(line[6:]) for line in writer.wfile.getvalue().decode().splitlines() if line.startswith("data: ")]
        self.assertEqual([f["type"] for f in frames], ["message_start", "content_block_start", "content_block_delta",
            "content_block_stop", "message_delta", "message_stop"])
        self.assertEqual(frames[0]["message"]["usage"], {"input_tokens": 32, "output_tokens": 0})
        self.assertEqual(frames[1]["content_block"]["name"], "Bash")
        self.assertEqual(json.loads(frames[2]["delta"]["partial_json"])["command"],
                         "printf 'TOOL_FACT=ochre-mole-392\\n' > claude-tool-effect.txt; cat claude-tool-effect.txt")
        self.assertEqual(frames[-2]["delta"]["stop_reason"], "tool_use")
        self.assertEqual(frames[-2]["usage"], {"output_tokens": 8})

    def test_final_stream_has_exact_answer_and_end_turn(self):
        writer = types.SimpleNamespace(wfile=io.BytesIO())
        body = request(2)
        C.reply(writer, body, C.classify(body), 3)
        frames = [json.loads(line[6:]) for line in writer.wfile.getvalue().decode().splitlines() if line.startswith("data: ")]
        self.assertEqual(frames[2]["delta"], {"type": "text_delta", "text": "CLAUDE_RESUMED_COMPLETE"})
        self.assertEqual(frames[-2]["delta"]["stop_reason"], "end_turn")

    def test_native_auxiliary_calls_are_not_history_or_conversation_turns(self):
        for system, kind, answer in [
            ("Analyze if this message indicates a new conversation topic.", "title", '{"isNewTopic":false,"title":null}'),
            ("Your task is to process Bash commands that an AI coding agent wants to run.", "bash-prefix", "none"),
            ("Extract any file paths that this command reads or modifies.", "file-paths", "<filepaths>\nclaude-tool-effect.txt\n</filepaths>")]:
            body = {"model": "claude-haiku-4-5-20251001", "system": [{"type": "text", "text": system}],
                    "messages": [{"role": "user", "content": C.PROMPTS[0]}], "stream": True}
            classified = C.classify(body)
            self.assertEqual(classified["kind"], kind)
            self.assertIsNone(classified["turn"])
            self.assertEqual(classified["missingHistory"], [])
            writer = types.SimpleNamespace(wfile=io.BytesIO())
            C.reply(writer, body, classified, 1)
            frames = [json.loads(line[6:]) for line in writer.wfile.getvalue().decode().splitlines() if line.startswith("data: ")]
            self.assertEqual(frames[2]["delta"]["text"], answer)


class AdministratorProbeTests(unittest.TestCase):
    def fixture(self, directory, remote=False, replacement=False, context="kind-sproozi-mobility-fixture"):
        root = Path(directory)
        (root / "docker").mkdir()
        kubeconfig = root / "kind.conf"
        kubeconfig.write_text("private fake configuration is never read by a worker")
        launch = {"sessionId": "fixture-session", "allocationId": "fixture-allocation", "operationId": "fixture-operation",
                  "threadId": "fixture-native", "epoch": 2 if remote else 1, "placement": "remote" if remote else "local", "harness": "claude-code"}
        agent = {"ID": "container-fixture", "Execution": "fixture-allocation"}
        (root / "docker" / "fixture.json").write_text(json.dumps({"launch": launch, "Agent": agent}))
        args = types.SimpleNamespace(kubeconfig=kubeconfig, kubectl=Path("kubectl"), namespace="sproozi-sessions", agent_namespace="sproozi-agents")
        calls = []
        observed = {"passed": True, "nativeUID": 1000, "capabilityDenials": {"kubernetesRead": {"httpStatus": "403"}},
                    "directDenials": {"ipv4": {"exitCode": 7, "httpStatus": "000"}}, "firewallMutationDenied": True}
        registration = {"sessionId": launch["sessionId"], "operationId": launch["operationId"], "epoch": launch["epoch"],
                        "harness": "claude-code", "nativeUuid": launch["threadId"],
                        "intendedRunName": "session-" + C.hashlib.sha256(launch["allocationId"].encode()).hexdigest()[:40]}
        registry = {"registration": registration, "runUid": "run-fixture", "podUid": "pod-fixture", "phase": "active"}
        pod = {"metadata": {"uid": "replacement" if replacement else "pod-fixture", "name": "fixture-pod"},
               "status": {"containerStatuses": [{"name": "agent", "state": {"running": {}}}]}}
        def run(command, label, data=None, timeout=None):
            calls.append((command, data))
            if command[-2:] == ["config", "current-context"]:
                return context.encode()
            if "inspect" in command:
                return json.dumps([{"Id": "replacement" if replacement else agent["ID"], "State": {"Running": True},
                                    "Config": {"Labels": {"sproozi.com/execution": "fixture-allocation"}}}]).encode()
            if "configmaps" in command:
                return json.dumps({"items": [{"data": {"registration.json": json.dumps(registry)}}]}).encode()
            if "pods" in command:
                return json.dumps({"items": [pod]}).encode()
            if "get" in command and "pod" in command:
                return json.dumps(pod).encode()
            if command[-1] == "status":
                return json.dumps({"state": "active", "threadId": "fixture-native"}).encode()
            if command[-1] == "/usr/local/bin/node":
                return json.dumps(observed).encode()
            raise AssertionError(command)
        proof = types.SimpleNamespace(args=args, config={"directory": str(root), "local": {"Name": "local", "Harness": "claude-code"},
            "dockerBinary": "docker", "dockerSocket": "/private/docker.sock", "dockerConfig": str(root)},
            id="fixture-session", report={"steps": []}, run=run, save=lambda: None)
        active = {"launch": launch, "incarnation": {"executionId": "run-fixture", "resourceUid": "pod-fixture"}}
        return proof, active, calls

    def test_local_exact_allocation_and_fixed_stdin_probe(self):
        with tempfile.TemporaryDirectory() as directory:
            proof, _, calls = self.fixture(directory)
            C.PROBES.NativeDenials(proof).run()
            node = [(command, data) for command, data in calls if command[-1] == "/usr/local/bin/node"]
            self.assertEqual(len(node), 1)
            self.assertEqual(node[0][0][-6:], ["exec", "--interactive", "--user", "1000", "container-fixture", "/usr/local/bin/node"])
            self.assertEqual(node[0][1], Path(__file__).with_name("native-denials.js").read_bytes())
            self.assertEqual(proof.report["steps"][0]["binding"], {"allocationId": "fixture-allocation", "containerId": "container-fixture"})

    def test_remote_exact_registry_pod_and_context(self):
        with tempfile.TemporaryDirectory() as directory:
            proof, active, calls = self.fixture(directory, remote=True)
            C.PROBES.NativeDenials(proof).run(active)
            node = [command for command, _ in calls if command[-1] == "/usr/local/bin/node"]
            self.assertEqual(len(node), 1)
            self.assertEqual(node[0][-7:], ["exec", "--stdin", "fixture-pod", "-c", "agent", "--", "/usr/local/bin/node"])
            self.assertEqual(node[0][node[0].index("--context") + 1], "kind-sproozi-mobility-fixture")
            self.assertEqual(proof.report["steps"][0]["binding"], {"allocationId": "fixture-allocation", "runUid": "run-fixture", "podUid": "pod-fixture"})

    def test_replacements_and_nonproof_contexts_reject_before_execution(self):
        for remote in (False, True):
            with tempfile.TemporaryDirectory() as directory:
                proof, active, calls = self.fixture(directory, remote=remote, replacement=True)
                with self.assertRaises(RuntimeError):
                    C.PROBES.NativeDenials(proof).run(active if remote else None)
                self.assertFalse(any(command[-1] == "/usr/local/bin/node" for command, _ in calls))
        with tempfile.TemporaryDirectory() as directory:
            proof, _, calls = self.fixture(directory, context="production")
            with self.assertRaisesRegex(RuntimeError, "dedicated mobility Kind"):
                C.PROBES.NativeDenials(proof)
            self.assertEqual(len(calls), 1)


if __name__ == "__main__":
    unittest.main()
