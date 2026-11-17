#!/usr/bin/env python3
"""Offline checks of the task's recorded-timestamp timing, on values taken from
run 20261117141846-64241 (its task-run output and the AgentRun's status shape)."""
import importlib.util
import json
from pathlib import Path
import unittest

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("claude_remote_timing", HERE / "timing.py")
T = importlib.util.module_from_spec(spec)
spec.loader.exec_module(T)

# 41-task-run.stdout: the deadline the executor set at submission, 30m ahead.
DEADLINE = "2026-11-17T14:57:03.041167173Z"
SUBMITTED = T.parse_time("2026-11-17T14:27:03.041167173Z")
UID = "aa189625-2f49-40d4-9517-19585539dcbc"


def runs(started="2026-11-17T14:27:03Z", completed="2026-11-17T14:27:11Z", uid=UID):
    status = {k: v for k, v in (("startedAt", started), ("completedAt", completed)) if v}
    return {"items": [{"metadata": {"uid": "other"}, "status": {"startedAt": "2026-11-17T14:00:00Z",
                                                                 "completedAt": "2026-11-17T14:09:00Z"}},
                      {"metadata": {"uid": uid}, "status": status}]}


class ServerTimingTest(unittest.TestCase):
    def test_interval_from_the_deadline_less_the_cap_to_the_providers_stamp(self):
        result = T.server_timing(DEADLINE, 1800, SUBMITTED + 3.217, waited=9.0)
        self.assertEqual(result, {"submittedAt": "2026-11-17T14:27:03.041Z",
                                  "firstModelRequestAt": "2026-11-17T14:27:06.258Z",
                                  "secondsToFirstModelRequest": 3.217})

    def test_does_not_move_with_the_polling_the_wall_wait_includes(self):
        fast = T.server_timing(DEADLINE, 1800, SUBMITTED + 3.217, waited=5.0)
        slow = T.server_timing(DEADLINE, 1800, SUBMITTED + 3.217, waited=7.6)
        self.assertEqual(fast, slow)

    def test_a_figure_outside_the_wait_is_dropped_with_a_note(self):
        for stamp in (SUBMITTED - 2, SUBMITTED + 600):
            result = T.server_timing(DEADLINE, 1800, stamp, waited=9.0)
            self.assertEqual(list(result), ["serverTimingNote"])
            self.assertIn("maxDuration", result["serverTimingNote"])

    def test_a_cap_the_deadline_was_not_set_from_is_dropped(self):
        self.assertEqual(list(T.server_timing(DEADLINE, 5400, SUBMITTED + 3, waited=9.0)), ["serverTimingNote"])


class RunTimingTest(unittest.TestCase):
    def test_whole_seconds_between_admission_and_completion_of_the_run(self):
        self.assertEqual(T.run_timing(runs(), UID), {"runAdmittedAt": "2026-11-17T14:27:03Z",
                                                     "runCompletedAt": "2026-11-17T14:27:11Z", "runSeconds": 8})

    def test_missing_run_or_time_gives_nothing(self):
        self.assertEqual(T.run_timing(runs(), "absent"), {})
        self.assertEqual(T.run_timing(runs(completed=None), UID), {})
        self.assertEqual(T.run_timing({}, UID), {})


class DurationTest(unittest.TestCase):
    def test_parses_the_examples_profile_cap(self):
        profile = json.loads((HERE / "../../../examples/portable-workflows/kubernetes/claude/profile.json").read_text())
        self.assertEqual(T.parse_duration(profile["maxDuration"]), 1800)

    def test_units_and_rejects(self):
        self.assertEqual(T.parse_duration("1h30m"), 5400)
        self.assertEqual(T.parse_duration("90s"), 90)
        for bad in ("", "30", "5d", "m30"):
            with self.assertRaises(ValueError):
                T.parse_duration(bad)


if __name__ == "__main__":
    unittest.main()
