#!/usr/bin/env python3
"""Offline checks for the Kind node clock comparison."""
import importlib.util
import unittest
from pathlib import Path
from unittest import mock

spec = importlib.util.spec_from_file_location(
    "kind_clock", Path(__file__).resolve().parent / "clock.py"
)
clock = importlib.util.module_from_spec(spec)
spec.loader.exec_module(clock)


class NodeSkewTest(unittest.TestCase):
    def test_skew_is_node_time_minus_host_midpoint(self):
        result = mock.Mock(stdout="1000\n")
        times = iter([3160.0, 3162.0])
        with mock.patch.object(clock.subprocess, "run", return_value=result) as run:
            skew = clock.node_skew("n", now=lambda: next(times))
        self.assertEqual(skew, -2161.0)
        self.assertEqual(run.call_args.args[0], ["docker", "exec", "n", "date", "+%s"])


class CheckNodeClocksTest(unittest.TestCase):
    def test_small_skew_passes_and_is_reported(self):
        skews = {"control": 0.4, "worker": -3.0}
        self.assertEqual(
            clock.check_node_clocks(skews, skew=skews.get),
            {"control": 0, "worker": -3},
        )

    def test_lagging_node_fails_with_cause_and_remedy(self):
        skews = {"control": 0.0, "worker": -2160.0}
        with self.assertRaisesRegex(
            RuntimeError, r"worker clock is 2160s behind the host.*expired certificate.*Docker Desktop"
        ):
            clock.check_node_clocks(skews, skew=skews.get)

    def test_leading_node_fails(self):
        with self.assertRaisesRegex(RuntimeError, "11s ahead of the host"):
            clock.check_node_clocks(["n"], skew=lambda _: 11.0)


if __name__ == "__main__":
    unittest.main()
