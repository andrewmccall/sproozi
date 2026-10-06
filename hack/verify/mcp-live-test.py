#!/usr/bin/env python3
"""Offline acceptance of the live verifier's safety and failure reports."""

import contextlib
import copy
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

SOURCE = Path(__file__).with_name("mcp-live.py")
spec = importlib.util.spec_from_file_location("mcp_live", SOURCE)
live = importlib.util.module_from_spec(spec)
spec.loader.exec_module(live)


class FakeCluster:
    """An isolated resource store; no external commands or model requests."""

    def __init__(self, failure):
        self.failure = failure
        self.objects = {("AgentRun", "unrelated"): {"metadata": {"name": "unrelated"}}}
        self.run = None
        self.pod = {"metadata": {"name": "sandbox"}, "status": {"phase": "Running"},
                    "spec": {"nodeName": "proof-worker", "containers": [
                        {"name": "agent", "image": "stock@sha256:123", "env": []}],
                        "volumes": [{"projected": {"sources": [{"serviceAccountToken": {}}]}}]}}

    def command(self, *args, **kwargs):
        output = ""
        code = 0
        if args[:2] == ("config", "current-context"):
            output = "kind-other" if self.failure == "context" else "kind-proof"
        elif args[0] == "delete":
            if self.failure == "cleanup-timeout":
                raise subprocess.TimeoutExpired("kubectl", 105)
            if self.failure == "cleanup-exit":
                code = 1
            else:
                kinds = {"agentrun": ["AgentRun"], "pod,networkpolicy": ["Pod", "NetworkPolicy"]}[args[1]]
                for kind in kinds:
                    self.objects.pop((kind, args[2]), None)
        elif args[0] == "logs":
            call = {"type": "mcp_tool_call", "server": "sproozi-kubernetes",
                    "tool": "kubernetes_list_pods", "status": "completed",
                    "arguments": {"namespace": "sproozi-demo"},
                    "result": {"content": [{"type": "text", "text": json.dumps({"kind": "PodList", "items": [
                        {"metadata": {"name": "mcp-observed-workload-fixture"}}]})}]}}
            output = json.dumps({"item": call}) + "\nSPROOZI_MCP_EXIT:0\nSPROOZI_MCP_AGENT_FINISHED\n"
        return subprocess.CompletedProcess(args, code, output, "PRIVATE_PROVIDER_DIAGNOSTIC")

    def create(self, obj):
        key = (obj["kind"], obj["metadata"]["name"])
        self.objects[key] = copy.deepcopy(obj)
        if obj["kind"] == "AgentRun":
            self.run = obj
            if self.failure == "ambiguous-create":
                raise subprocess.TimeoutExpired("kubectl", 45)
        if obj["kind"] == "Pod":
            raise RuntimeError("Witness creation failed")

    def get(self, resource, name=None, namespace=None):
        if resource == "nodes":
            return {"items": [{"metadata": {"name": "proof-control-plane"}, "status": {"addresses": [
                {"type": "InternalIP", "address": "192.0.2.1"}]}}, {"metadata": {"name": "proof-worker"}}]}
        if resource == "agentrun":
            if self.failure in {"read-run", "cleanup-timeout", "cleanup-exit"}:
                raise RuntimeError("Run inspection failed")
            return dict(self.run, metadata=dict(self.run["metadata"], uid="12345678-fixture"),
                        status={"identity": {"sandboxName": "sandbox"}})
        if resource == "pod":
            return copy.deepcopy(self.pod)
        if resource == "service":
            return {"spec": {"clusterIP": "192.0.2.2"}}
        if resource == "networkpolicies":
            return {"items": [{"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
                "metadata": {"labels": {live.RUN_LABEL: "12345678-fixture"}}, "spec": {}}]}
        raise AssertionError(resource)

    def exec(self, *args):
        return subprocess.CompletedProcess(args, 0, "codex fixture", "")

    def mcp(self, *args):
        return "200", "200", {"error": {"code": -32602}}


class VerifierTests(unittest.TestCase):
    def exercise(self, failure):
        cluster = FakeCluster(failure)
        with tempfile.TemporaryDirectory() as directory:
            config = Path(directory, "kubeconfig")
            config.touch()
            report = Path(directory, "nested/report.json")
            with patch.object(live, "Cluster", return_value=cluster), patch.object(
                live.subprocess, "check_output", return_value="proof\n"
            ), contextlib.redirect_stdout(io.StringIO()):
                with self.assertRaises((RuntimeError, subprocess.TimeoutExpired)):
                    live.main(["--kubeconfig", str(config), "--cluster", "proof", "--report", str(report)])
            result = json.loads(report.read_text())
            self.assertEqual(result["outcome"], "failed")
            self.assertIn("finishedAt", result)
            self.assertNotIn("PRIVATE_PROVIDER_DIAGNOSTIC", report.read_text())
            return cluster, result

    def test_cli_requires_an_existing_explicit_kubeconfig(self):
        with tempfile.TemporaryDirectory() as directory:
            report = Path(directory, "report.json")
            result = subprocess.run(["python3", str(SOURCE), "--kubeconfig", str(Path(directory, "missing")),
                                     "--cluster", "proof", "--report", str(report)], capture_output=True, text=True)
            self.assertEqual(result.returncode, 2)
            self.assertIn("Supply an existing kubeconfig", result.stderr)
            self.assertFalse(report.exists())

    def test_wrong_cluster_reports_failure_without_mutation(self):
        cluster, report = self.exercise("context")
        self.assertEqual(list(cluster.objects), [("AgentRun", "unrelated")])
        self.assertEqual(report["error"], "The explicit kubeconfig must select the named dedicated Kind cluster")

    def test_failed_run_inspection_cleans_up_without_uid(self):
        cluster, report = self.exercise("read-run")
        self.assertEqual(list(cluster.objects), [("AgentRun", "unrelated")])
        self.assertEqual(report["error"], "Run inspection failed")

    def test_ambiguous_create_is_cleaned_up(self):
        cluster, report = self.exercise("ambiguous-create")
        self.assertEqual(list(cluster.objects), [("AgentRun", "unrelated")])
        self.assertEqual(report["error"], "TimeoutExpired")

    def test_partial_witness_failure_cleans_up_both_resources(self):
        cluster, report = self.exercise("witness")
        self.assertEqual(list(cluster.objects), [("AgentRun", "unrelated")])
        self.assertEqual(report["error"], "Witness creation failed")
        self.assertTrue(report["checks"]["stockCodexMCP"])

    def test_cleanup_failures_preserve_original_error(self):
        for failure in ("cleanup-timeout", "cleanup-exit"):
            with self.subTest(failure=failure):
                cluster, report = self.exercise(failure)
                self.assertEqual(report["error"], "Run inspection failed")
                self.assertEqual(report["cleanupError"], "Verifier cleanup failed; inspect the named proof resources")
                self.assertEqual(len(cluster.objects), 2)

    def test_all_container_secret_sources_are_rejected(self):
        for field in ("containers", "initContainers", "ephemeralContainers"):
            for source in ({"env": [{"valueFrom": {"secretKeyRef": {"name": "provider"}}}]},
                           {"envFrom": [{"secretRef": {"name": "provider"}}]}):
                with self.subTest(field=field, source=source), self.assertRaisesRegex(RuntimeError, "must not reference Secrets"):
                    live.require_no_sandbox_secrets({field: [{"name": "auxiliary", **source}]})

    def test_volume_and_image_pull_secret_sources_are_rejected(self):
        for value in ({"volumes": [{"secret": {"secretName": "provider"}}]},
                      {"volumes": [{"projected": {"sources": [{"secret": {"name": "provider"}}]}}]},
                      {"imagePullSecrets": [{"name": "provider"}]}):
            with self.subTest(value=value), self.assertRaises(RuntimeError):
                live.require_no_sandbox_secrets(value)

    def test_run_identity_and_public_config_are_accepted(self):
        live.require_no_sandbox_secrets({"containers": [{"envFrom": [{"configMapRef": {"name": "public"}}]}],
            "volumes": [{"projected": {"sources": [{"serviceAccountToken": {}}, {"configMap": {"name": "trust"}}]}}]})


if __name__ == "__main__":
    unittest.main()
