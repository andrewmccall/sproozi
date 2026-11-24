#!/usr/bin/env python3
"""Offline checks of run.sh's wait for a namespace's default ServiceAccount,
with a stand-in kubectl."""
from pathlib import Path
import os
import subprocess
import tempfile
import unittest

HERE = Path(__file__).resolve().parent
SCRIPT = HERE / "service-account.sh"


def wait(namespace, kubectl_exit, limit=None):
    """Runs the wait with a kubectl that records its arguments and exits with
    kubectl_exit; returns the process and the recorded arguments."""
    with tempfile.TemporaryDirectory() as directory:
        directory = Path(directory)
        stub = directory / "kubectl"
        stub.write_text('#!/bin/sh\necho "$@" >"%s"\nexit %d\n' % (directory / "args", kubectl_exit))
        stub.chmod(0o755)
        env = {"PATH": str(directory) + os.pathsep + os.environ["PATH"]}
        if limit is not None:
            env["SERVICE_ACCOUNT_WAIT_SECONDS"] = str(limit)
        result = subprocess.run(["bash", "-c", 'source "$1"; wait_for_default_service_account "$2"', "_",
                                 str(SCRIPT), namespace], env=env, capture_output=True, text=True, timeout=30)
        args = directory / "args"
        return result, args.read_text().strip() if args.exists() else None


class ServiceAccountWaitTest(unittest.TestCase):
    def test_waits_for_the_service_account_to_be_created_within_the_limit(self):
        result, args = wait("sproozi-agents", 0)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(args, "wait --for=create serviceaccount/default --namespace sproozi-agents --timeout=60s")

    def test_limit_is_configurable(self):
        _, args = wait("sproozi-agents", 0, limit=5)
        self.assertTrue(args.endswith("--timeout=5s"), args)

    def test_failure_names_what_did_not_appear_and_the_limit(self):
        result, _ = wait("sproozi-agents", 1, limit=7)
        self.assertEqual(result.returncode, 1)
        self.assertIn("ServiceAccount default did not appear in namespace sproozi-agents within 7s", result.stderr)

    def test_run_script_waits_before_the_cluster_objects_are_applied(self):
        script = (HERE / "run.sh").read_text()
        self.assertIn("source hack/verify/claude-remote-kind/service-account.sh", script)
        self.assertLess(script.index('wait_for_default_service_account "$ns"'),
                        script.index('kubectl apply --server-side -f "$work/cluster.yaml"'))
        for namespace in ("sproozi-system", "sproozi-agents", '"$sessions"', "sproozi-fixture"):
            self.assertIn(namespace, script[script.index("for ns in sproozi-system"):])


if __name__ == "__main__":
    unittest.main()
