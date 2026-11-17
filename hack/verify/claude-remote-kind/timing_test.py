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


# Run 2 of the stalled-task investigation (its report.json, scenarios.task):
# the controller admitted the AgentRun at 21:32:42 and moved it to Succeeded at
# 21:32:49, about two seconds after the proof's poll saw the task retained
# (21:32:42.799 + 4.4 s). The gateway log's runUID is the status view's
# incarnation.executionId, so the lookup key was right; the list was read
# before status.completedAt was set.
RUN2_UID = "16f33429-8e1b-485e-a3fd-08112123fe09"
RUN2_DEADLINE = "2026-11-17T22:02:42.799Z"  # 30m after submittedAt 21:32:42.799
RUN2_REQUEST = T.parse_time("2026-11-17T21:32:45.706Z")


def run2(started="2026-11-17T21:32:42Z", completed="2026-11-17T21:32:49Z"):
    status = {k: v for k, v in (("startedAt", started), ("completedAt", completed)) if v}
    return {"items": [{"metadata": {"uid": "someone-else"}, "status": {}},
                      {"metadata": {"uid": RUN2_UID}, "status": status}]}


class RunTimingTest(unittest.TestCase):
    def test_whole_seconds_between_admission_and_completion_of_the_run(self):
        self.assertEqual(T.run_timing(run2(), RUN2_UID),
                         ({"runAdmittedAt": "2026-11-17T21:32:42Z", "runCompletedAt": "2026-11-17T21:32:49Z",
                           "runSeconds": 7}, True))

    def test_no_such_run_is_noted(self):
        fields, complete = T.run_timing(run2(), "absent")
        self.assertEqual(fields, {"runTimingNote": "No AgentRun has UID absent among the 2 listed"})
        self.assertTrue(complete)
        self.assertIn("among the 0 listed", T.run_timing({}, "absent")[0]["runTimingNote"])

    def test_run_without_started_at_is_noted_and_final(self):
        self.assertEqual(T.run_timing(run2(started=None), RUN2_UID),
                         ({"runTimingNote": "The AgentRun was found without status.startedAt"}, True))

    def test_run_without_completed_at_is_noted_and_may_yet_complete(self):
        self.assertEqual(T.run_timing(run2(completed=None), RUN2_UID),
                         ({"runTimingNote": "The AgentRun was found without status.completedAt"}, False))


class AwaitRunTimingTest(unittest.TestCase):
    def clock(self):
        self.now = 0.0
        self.sleeps = 0

        def sleep(seconds):
            self.now += seconds
            self.sleeps += 1
        return (lambda: self.now), sleep

    def test_waits_for_the_controller_to_record_completion(self):
        # The proof read the list as it saw retained: Running, no completedAt.
        clock, sleep = self.clock()
        reads = [run2(completed=None), run2(completed=None), run2()]
        fields = T.await_run_timing(lambda: reads.pop(0), RUN2_UID, limit=30, clock=clock, sleep=sleep)
        self.assertEqual(fields["runSeconds"], 7)
        self.assertNotIn("runTimingNote", fields)
        self.assertEqual(self.sleeps, 2)

    def test_gives_up_at_the_limit_and_says_what_was_missing(self):
        clock, sleep = self.clock()
        fields = T.await_run_timing(lambda: run2(completed=None), RUN2_UID, limit=5, clock=clock, sleep=sleep)
        self.assertEqual(fields, {"runTimingNote": "The AgentRun was found without status.completedAt within 5s"})
        self.assertEqual(self.sleeps, 5)

    def test_a_missing_run_is_not_waited_for(self):
        clock, sleep = self.clock()
        fields = T.await_run_timing(lambda: run2(), "absent", clock=clock, sleep=sleep)
        self.assertIn("No AgentRun has UID absent", fields["runTimingNote"])
        self.assertEqual(self.sleeps, 0)

    def test_a_failed_list_call_is_noted(self):
        def read():
            raise json.JSONDecodeError("Expecting value", "", 0)
        fields = T.await_run_timing(read, RUN2_UID)
        self.assertTrue(fields["runTimingNote"].startswith("The AgentRun list call failed: JSONDecodeError"))

    def test_the_limit_is_named(self):
        self.assertEqual(T.RUN_COMPLETION_WAIT_SECONDS, 30)


class ServerTimingFromTest(unittest.TestCase):
    started = {"deadline": RUN2_DEADLINE}

    def test_run2_figure(self):
        self.assertEqual(T.server_timing_from(self.started, {"at": RUN2_REQUEST}, 1800, 6.4),
                         {"submittedAt": "2026-11-17T21:32:42.799Z", "firstModelRequestAt": "2026-11-17T21:32:45.706Z",
                          "secondsToFirstModelRequest": 2.907})

    def test_each_missing_input_is_noted(self):
        unreadable = "the profile's maxDuration is unreadable: x"
        for args, text in (((self.started, {"at": RUN2_REQUEST}, unreadable, 6), unreadable),
                           (({}, {"at": RUN2_REQUEST}, 1800, 6), "the run output has no deadline"),
                           ((self.started, {}, 1800, 6), "recorded no first request time"),
                           ((self.started, None, 1800, 6), "recorded no first request time")):
            result = T.server_timing_from(*args)
            self.assertEqual(list(result), ["serverTimingNote"])
            self.assertIn(text, result["serverTimingNote"])


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
