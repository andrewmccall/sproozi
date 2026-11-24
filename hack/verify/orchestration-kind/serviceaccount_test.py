#!/usr/bin/env python3
"""Offline checks that no bare Pod is applied before its new namespace's
default ServiceAccount exists, with kubectl replaced by a recorder."""
import json
import sys
import unittest
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import managed  # noqa: E402
import serviceaccount  # noqa: E402
import verify  # noqa: E402


class Recorder:
    """Stands in for verify.k: records each call, and fails a wait if told to."""

    def __init__(self, fail_wait=False):
        self.calls, self.fail_wait = [], fail_wait

    def k(self, *args, input=None, **_):
        self.calls.append((args, json.loads(input) if input else None))
        if self.fail_wait and args[0] == "wait":
            raise RuntimeError("kubectl wait\ntimed out waiting for the condition")
        return ""


def kinds(call):
    body = call[1]
    return [i["kind"] for i in body["items"]] if body.get("kind") == "List" else [body["kind"]]


class WaitTest(unittest.TestCase):
    def test_waits_for_creation_in_the_namespace_within_the_bound(self):
        recorder = Recorder()
        serviceaccount.wait_for_default_service_account(recorder.k, "ns")
        self.assertEqual(recorder.calls, [(("wait", "--for=create", "serviceaccount/default", "-n", "ns",
                                            "--timeout=60s"), None)])

    def test_failure_names_what_did_not_appear(self):
        with self.assertRaises(RuntimeError) as caught:
            serviceaccount.wait_for_default_service_account(Recorder(fail_wait=True).k, "ns", seconds=7)
        self.assertIn("ServiceAccount default did not appear in namespace ns within 7s", str(caught.exception))
        self.assertIn("timed out waiting", str(caught.exception))


class OrderTest(unittest.TestCase):
    def test_caller_pod_is_applied_after_its_namespace_and_service_account(self):
        recorder = Recorder()
        managed.apply_caller(recorder, "img", "ca", "token")
        steps = [("apply", kinds(c)) if c[0][0] == "apply" else (c[0][0], None) for c in recorder.calls]
        self.assertEqual(steps, [("apply", ["Namespace"]), ("wait", None),
                                 ("apply", ["ConfigMap", "Secret", "Pod"])])
        self.assertEqual(recorder.calls[1][0][4], managed.CALLER_NAMESPACE)

    def test_a_missing_service_account_stops_before_the_pod(self):
        recorder = Recorder(fail_wait=True)
        with self.assertRaises(RuntimeError):
            managed.apply_caller(recorder, "img", "ca", "token")
        self.assertEqual([c[0][0] for c in recorder.calls], ["apply", "wait"])

    def test_kubernetes_worker_pod_is_applied_after_its_namespace_and_service_account(self):
        events = []

        class Stop(Exception):
            pass

        def apply(*objects):
            events.append(("apply", [o["kind"] for o in objects]))
            if objects[0]["kind"] == "Pod":
                raise Stop

        def k(*args, **_):
            events.append((args[0], args[4] if args[0] == "wait" else None))
            return ""

        saved = verify.apply, verify.k
        verify.apply, verify.k = apply, k
        try:
            with self.assertRaises(Stop):
                verify.kubernetes_worker_proof({"clients": {"image": "img"}})
        finally:
            verify.apply, verify.k = saved
        self.assertEqual(events, [("apply", ["Namespace"]), ("wait", "sproozi-kubernetes-proof"),
                                  ("apply", ["Pod"])])


if __name__ == "__main__":
    unittest.main()
