"""Offline protocol tests; genuine native execution remains the live verifier's job."""
import contextlib
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import threading
from types import SimpleNamespace
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("mobility_host", Path(__file__).with_name("host.py"))
HOST = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(HOST)


def message(role, text):
    return {"role": role, "content": [{"type": "input_text", "text": text}]}


def request(turn):
    items = [message("developer", HOST.INSTRUCTIONS), message("user", HOST.PROMPTS[0])]
    if turn:
        items += [message("assistant", HOST.ANSWERS[0])]
    if turn == 2:
        items += [message("user", HOST.PROMPTS[1]), message("assistant", HOST.ANSWERS[1])]
    if turn:
        items += [message("user", HOST.ALL_PROMPTS[turn])]
    items.insert(0, {"type": "additional_tools", "role": "developer", "tools": [
        {"type": "namespace", "name": "functions", "tools": [
            {"type": "custom", "name": "exec", "description": "tools.exec_command(args)"}]}]})
    return {"model": "fixture-model", "stream": True, "input": items}


def events(writer):
    return [json.loads(line[6:]) for line in writer.getvalue().decode().splitlines() if line.startswith("data: ")]


class FixtureTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.out = Path(self.temporary.name)
        HOST.TUI.OUT, HOST.TUI.REQUESTS, HOST.TUI.EVIDENCE = self.out, [], {}
        self.stop = threading.Event()
        self.args = SimpleNamespace(instructions=HOST.INSTRUCTIONS, hold_timeout=1)

    def tearDown(self):
        self.stop.set()
        self.temporary.cleanup()

    def handler(self, body, writer=None, auth=HOST.TOKEN):
        kind = HOST.provider_class(self.args, self.out, self.stop)
        handler = kind.__new__(kind)
        raw = json.dumps(body).encode()
        handler.headers = {"Content-Length": str(len(raw)), "Authorization": "Bearer " + auth}
        handler.rfile, handler.wfile, handler.path = io.BytesIO(raw), writer or io.BytesIO(), "/v1/responses"
        handler.status = None
        handler.send_response = lambda status: setattr(handler, "status", status)
        handler.send_header = lambda *_: None
        handler.end_headers = lambda: None
        handler.send_error = lambda status, *_: setattr(handler, "status", status)
        return handler

    def arm(self, turn=4):
        args = SimpleNamespace(output_directory=self.out, operation="arm", prompt_index=turn, reply_phase="answer")
        with contextlib.redirect_stdout(io.StringIO()):
            HOST.control(args)
        return json.loads((self.out / "hold.json").read_text())["id"]

    def release(self, hold_id):
        with contextlib.redirect_stdout(io.StringIO()):
            HOST.control(SimpleNamespace(output_directory=self.out, operation="release", hold_id=hold_id))

    def test_existing_turns_and_titles_keep_prompt_identity_and_usage(self):
        for turn in [2, 0, 1, 2]:
            handler = self.handler(request(turn))
            handler.do_POST()
            self.assertEqual(handler.status, 200)
            completed = events(handler.wfile)[-1]
            self.assertEqual(completed["type"], "response.completed")
            self.assertEqual(completed["response"]["usage"]["total_tokens"], 15)
            self.assertEqual(HOST.TUI.REQUESTS[-1]["turn"], turn)
        title = {"model": "fixture-model", "input": [message("user", "Generate a concise, single-line task title now")]}
        handler = self.handler(title)
        handler.do_POST()
        self.assertEqual(HOST.TUI.REQUESTS[-1]["kind"], "title")
        self.assertIsNone(HOST.TUI.REQUESTS[-1]["turn"])
        self.assertEqual(handler.status, 200)
        denied = self.handler(title, auth="wrong")
        denied.do_POST()
        self.assertEqual(denied.status, 403)

    def test_tool_call_then_actual_matching_successful_output(self):
        first = self.handler(request(3))
        first.do_POST()
        item = events(first.wfile)[-1]["response"]["output"][0]
        self.assertEqual((item["type"], item["namespace"], item["name"]), ("custom_tool_call", "functions", "exec"))
        self.assertIn("tools.exec_command", item["input"])
        self.assertIn("native-tool-effect.txt", item["input"])
        followup = request(3)
        followup["input"] += [item, {"type": "custom_tool_call_output", "call_id": item["call_id"],
                                    "output": json.dumps({"exit_code": 0, "output": HOST.TOOL_MARKER + "\n"})}]
        second = self.handler(followup)
        second.do_POST()
        self.assertEqual(HOST.TUI.REQUESTS[-1]["kind"], "tool-followup")
        self.assertTrue(HOST.TUI.REQUESTS[-1]["toolOutputValidated"])
        self.assertEqual(events(second.wfile)[-1]["response"]["output"][0]["content"][0]["text"], "TUI_TOOL_COMPLETE")
        followup["input"][-1]["output"] = json.dumps({"exit_code": 1, "output": HOST.TOOL_MARKER})
        failed = self.handler(followup)
        failed.do_POST()
        self.assertEqual(failed.status, 400)
        self.assertEqual(failed.wfile.getvalue(), b"")
        self.assertEqual(len(HOST.TUI.REQUESTS), 2)
        # Native functions.exec can nest text() JSON within content blocks.
        followup["input"][-1]["output"] = json.dumps([{"type": "text", "text": json.dumps({
            "exit_code": 0, "output": HOST.TOOL_MARKER + "\n"})}])
        self.assertTrue(HOST.classify_request(followup)["toolOutputValidated"])

    def test_old_tool_output_does_not_finish_new_same_prompt(self):
        body = request(3)
        body["input"].insert(-1, {"type": "custom_tool_call_output", "call_id": "mobility_exec_tool",
                                  "output": json.dumps({"exit_code": 0, "output": HOST.TOOL_MARKER})})
        self.assertEqual(HOST.classify_request(body)["phase"], "tool-call")
        body["input"] = [item for item in body["input"] if item.get("type") != "additional_tools"]
        with self.assertRaisesRegex(ValueError, "not advertised"):
            HOST.classify_request(body)

    def test_exact_host_release_required_before_completed_stream(self):
        hold_id = self.arm()
        handler = self.handler(request(4))
        held = threading.Event()
        persist = HOST.TUI.persist
        def record():
            persist()
            if HOST.TUI.REQUESTS and HOST.TUI.REQUESTS[-1].get("held"):
                held.set()
        with patch.object(HOST.TUI, "persist", record):
            worker = threading.Thread(target=handler.do_POST)
            worker.start()
            self.assertTrue(held.wait(1))
            self.assertEqual([event["type"] for event in events(handler.wfile)], ["response.created"])
            self.assertFalse(HOST.TUI.REQUESTS[-1]["completed"])
            self.assertNotIn("usage", HOST.TUI.REQUESTS[-1])
            with self.assertRaisesRegex(RuntimeError, "exact armed hold ID"):
                self.release("stale-hold-id")
            self.release(hold_id)
            worker.join(1)
            self.assertFalse(worker.is_alive())
        self.assertTrue(HOST.TUI.REQUESTS[-1]["released"])
        self.assertTrue(HOST.TUI.REQUESTS[-1]["completed"])
        self.assertEqual(events(handler.wfile)[-1]["type"], "response.completed")

    def test_cancelled_or_timed_out_hold_never_synthesizes_completion(self):
        self.arm()
        self.stop.set()
        handler = self.handler(request(4))
        handler.do_POST()
        self.assertEqual([event["type"] for event in events(handler.wfile)], ["response.created"])
        self.assertFalse(HOST.TUI.REQUESTS[-1]["completed"])
        self.assertEqual(HOST.TUI.REQUESTS[-1]["streamError"], "RuntimeError")
        self.stop.clear()
        self.args.hold_timeout = 0
        handler = self.handler(request(4))
        handler.do_POST()
        self.assertEqual(HOST.TUI.REQUESTS[-1]["streamError"], "TimeoutError")
        self.assertNotIn("usage", HOST.TUI.REQUESTS[-1])

    def test_broken_socket_cannot_be_recorded_as_completed_usage(self):
        class Broken(io.BytesIO):
            def write(self, content):
                if b'response.completed' in content:
                    raise BrokenPipeError()
                return super().write(content)
        handler = self.handler(request(0), writer=Broken())
        handler.do_POST()
        self.assertFalse(HOST.TUI.REQUESTS[-1]["completed"])
        self.assertNotIn("usage", HOST.TUI.REQUESTS[-1])
        self.assertEqual(HOST.TUI.REQUESTS[-1]["streamError"], "BrokenPipeError")


if __name__ == "__main__":
    unittest.main()
