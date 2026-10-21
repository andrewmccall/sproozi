#!/usr/bin/env python3
"""Offline checks for hack/mcp-tools.py against local fake MCP servers."""
import contextlib
import importlib.util
import io
import json
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import sys
import tempfile
import threading
import unittest
from unittest import mock

SPEC = importlib.util.spec_from_file_location("mcp_tools", Path(__file__).with_name("mcp-tools.py"))
TOOLS = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(TOOLS)

LISTED = {"tools": [{"name": "ask", "inputSchema": {"type": "object", "required": ["q"],
                                                  "properties": {"q": {}, "page": {}}}}]}


class Handler(BaseHTTPRequestHandler):
    mode = "json"

    def log_message(self, *args):
        pass

    def reply(self, body, kind):
        data = body.encode()
        self.send_response(200)
        self.send_header("Content-Type", kind)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        if self.mode != "legacy":
            self.send_response(405)
            self.end_headers()
            return
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        self.wfile.write(b"event: endpoint\ndata: /message?sessionId=1\n\n")
        self.wfile.flush()

    def do_POST(self):
        message = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        if self.mode == "legacy":
            self.send_response(405)
            self.end_headers()
            return
        if "id" not in message:
            self.send_response(202)
            self.end_headers()
            return
        result = {"serverInfo": {"name": "Fake", "version": "1"}} if message["method"] == "initialize" else LISTED
        body = json.dumps({"jsonrpc": "2.0", "id": message["id"], "result": result})
        if self.mode == "sse":
            self.reply("event: message\ndata: " + body + "\n\n", "text/event-stream")
        else:
            self.reply(body, "application/json")


class McpToolsTest(unittest.TestCase):
    def run_against(self, mode, *extra):
        Handler.mode = mode
        server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=server.serve_forever, daemon=True).start()
        self.addCleanup(server.shutdown)
        out, err = io.StringIO(), io.StringIO()
        url = "http://127.0.0.1:%d/mcp" % server.server_address[1]
        with mock.patch.object(sys, "argv", ["mcp-tools.py", url, *extra]), contextlib.redirect_stdout(out), \
                contextlib.redirect_stderr(err):
            try:
                TOOLS.main()
            except RuntimeError as error:
                return "", str(error)
            except SystemExit as exit:
                return out.getvalue(), err.getvalue() + "exit %s" % exit.code
        return out.getvalue(), err.getvalue()

    def test_lists_tools_from_json_and_sse_replies(self):
        for mode in ("json", "sse"):
            out, _ = self.run_against(mode)
            self.assertIn("Fake 1 offers 1 tools:", out)
            self.assertIn("ask(q, page?)", out)

    def test_names_a_legacy_sse_only_url(self):
        _, err = self.run_against("legacy")
        self.assertIn("legacy HTTP+SSE transport", err)


class PolicyCheckTest(unittest.TestCase):
    def test_reports_unknown_tools_arguments_and_forbidden_required_arguments(self):
        scope = {"ask": {"arguments": {"type": "object", "required": ["question"], "additionalProperties": False,
                                       "properties": {"question": {}}}},
                 "ask_question": {}}
        problems = TOOLS.check_policy(LISTED["tools"], scope)
        self.assertEqual(problems, [
            "tool 'ask' has no argument 'question'; its arguments are page, q",
            "tool 'ask' requires argument 'q', which the policy's additionalProperties: false forbids, "
            "so every call would fail",
            "tool 'ask_question' is not offered; the server offers ask"])
        self.assertEqual(TOOLS.check_policy(LISTED["tools"], {"ask": {"arguments": {"properties": {"q": {}}}}}), [])

    def test_policy_option_exits_non_zero_on_a_mismatch(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        policy = Path(directory.name) / "policy.json"
        for tools, ok in [({"ask": {}}, True), ({"ask_question": {}}, False)]:
            policy.write_text(json.dumps({"spec": {"mcpServers": {"fake": {"tools": tools}}}}))
            out, err = McpToolsTest.run_against(self, "json", "--policy", str(policy))
            if ok:
                self.assertIn("The policy's 1 fake tools match", out)
            else:
                self.assertIn("tool 'ask_question' is not offered", err)
                self.assertIn("exit 1", err)


if __name__ == "__main__":
    unittest.main()
