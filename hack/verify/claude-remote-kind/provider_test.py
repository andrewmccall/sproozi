#!/usr/bin/env python3
"""Offline checks of the Claude remote fixture provider and renderer; no
cluster, listener or model."""
import importlib.util
import json
from pathlib import Path
import unittest

HERE = Path(__file__).resolve().parent


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


P = module("claude_remote_provider_test", HERE / "provider.py")


def body(prompt, *later, tools=("Bash", "Skill"), history=()):
    messages = [{"role": "user", "content": text} if i % 2 == 0 else
                {"role": "assistant", "content": [{"type": "text", "text": text}]}
                for i, text in enumerate(history)]
    messages.append({"role": "user", "content": [{"type": "text", "text": prompt}]})
    messages += later
    return {"model": P.MODEL, "stream": True, "messages": messages, "tools": [{"name": t} for t in tools]}


def call(name, ident, arguments=None):
    return {"role": "assistant", "content": [{"type": "tool_use", "id": ident, "name": name, "input": arguments or {}}]}


def result(ident, text, error=False):
    return {"role": "user", "content": [{"type": "tool_result", "tool_use_id": ident, "content": text,
                                         "is_error": error}]}


class ProviderTests(unittest.TestCase):
    def test_each_scenario_answers_by_its_prompt(self):
        for name in ("first", "exit", "task"):
            record, reply = P.plan(body(P.SCENARIOS[name]["prompt"]))
            self.assertEqual((record["scenario"], reply), (name, ("text", P.SCENARIOS[name]["answer"])))

    def test_a_resumed_turn_needs_the_first_turn_history(self):
        with self.assertRaises(P.Refused):
            P.plan(body(P.SCENARIOS["resumed"]["prompt"]))
        first = P.SCENARIOS["first"]
        record, reply = P.plan(body(P.SCENARIOS["resumed"]["prompt"], history=(first["prompt"], first["answer"])))
        self.assertEqual(record["missingHistory"], [])
        self.assertEqual(reply, ("text", P.SCENARIOS["resumed"]["answer"]))

    def test_the_upload_turn_reads_the_uploaded_file_with_bash(self):
        prompt = P.SCENARIOS["upload"]["prompt"]
        record, reply = P.plan(body(prompt))
        self.assertEqual((record["phase"], reply), ("read-upload", ("tool", "Bash", P.UPLOAD_COMMAND)))
        record, reply = P.plan(body(prompt, call("Bash", "t1"), result("t1", P.UPLOAD_MARKER + "\n")))
        self.assertTrue(record["uploadMarkerSeen"])
        self.assertEqual(reply, ("text", P.SCENARIOS["upload"]["answer"]))
        record, _ = P.plan(body(prompt, call("Bash", "t1"), result("t1", "cat: no such file", True)))
        self.assertFalse(record["uploadMarkerSeen"])

    def test_the_tools_turn_waits_for_diagnose_then_loads_the_skill(self):
        prompt = P.SCENARIOS["tools"]["prompt"]
        record, reply = P.plan(body(prompt))
        self.assertEqual((record["phase"], reply[1]), ("wait", "Bash"))
        tools = ("Bash", "Skill", P.DIAGNOSE)
        record, reply = P.plan(body(prompt, call("Bash", "w1"), result("w1", "waited"), tools=tools))
        self.assertEqual((record["phase"], reply), ("diagnose", ("tool", P.DIAGNOSE, {})))
        steps = [call("Bash", "w1"), result("w1", "waited"), call(P.DIAGNOSE, "d1"), result("d1", '{"status":"not_observed"}')]
        record, reply = P.plan(body(prompt, *steps, tools=tools))
        self.assertEqual((record["phase"], reply), ("skill", ("tool", "Skill", {"skill": "profile:" + P.PROFILE_SKILL})))
        self.assertTrue(record["diagnoseResult"])
        steps += [call("Skill", "s1"), result("s1", "Launching skill"),
                  {"role": "user", "content": [{"type": "text", "text": "The profile marker is " + P.PROFILE_MARKER}]}]
        # The skill's body arrives as a user message; the newest prompt is
        # then the skill text, so the scenario comes from the turn's prompt.
        record, reply = P.plan(body(prompt, *steps, tools=tools))
        self.assertEqual(record["phase"], "answer")
        self.assertTrue(record["profileSkillLoaded"])
        self.assertEqual(reply, ("text", P.SCENARIOS["tools"]["answer"]))

    def test_the_tools_turn_gives_up_when_diagnose_is_never_offered(self):
        prompt = P.SCENARIOS["tools"]["prompt"]
        steps = []
        for i in range(P.MAX_WAITS):
            steps += [call("Bash", "w%d" % i), result("w%d" % i, "waited")]
        with self.assertRaises(P.Refused):
            P.plan(body(prompt, *steps))

    def test_auxiliary_requests_are_answered_unscripted(self):
        record, reply = P.plan({"model": "claude-haiku-4-5", "system": [{"type": "text", "text":
                                "Analyze if this message indicates a new conversation topic."}], "messages": []})
        self.assertEqual(record["kind"], "auxiliary")
        self.assertEqual(json.loads(reply[1]), {"isNewTopic": False, "title": None})

    def test_unscripted_prompts_are_refused(self):
        with self.assertRaises(P.Refused):
            P.plan(body("Something else"))

    def test_streamed_events_carry_the_reply(self):
        msg = P.message({"model": P.MODEL}, ("tool", "Skill", {"skill": "profile:x"}), 3)
        events = list(P.events(msg))
        self.assertEqual([e[0] for e in events][0], "message_start")
        self.assertEqual(json.loads(events[2][1]["delta"]["partial_json"]), {"skill": "profile:x"})
        self.assertEqual(events[4][1]["delta"]["stop_reason"], "tool_use")


if __name__ == "__main__":
    unittest.main()
