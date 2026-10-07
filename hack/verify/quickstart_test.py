"""Boundary regressions for the Kind quickstart driver and owned cleanup."""

import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("quickstart", Path(__file__).with_name("quickstart.py"))
quickstart = importlib.util.module_from_spec(spec)
spec.loader.exec_module(quickstart)


def installed_crds():
    return {
        "agentruns.sproozi.com": {"group": "sproozi.com", "kind": "AgentRun"},
        "agentruntimes.sproozi.com": {"group": "sproozi.com", "kind": "AgentRuntime"},
        "agentpolicies.sproozi.com": {"group": "sproozi.com", "kind": "AgentPolicy"},
        "agenttemplates.sproozi.com": {"group": "sproozi.com", "kind": "AgentTemplate"},
        "networkpolicies.crd.projectcalico.org": {"group": "crd.projectcalico.org", "kind": "NetworkPolicy"},
        "adminnetworkpolicies.policy.networking.k8s.io": {"group": "policy.networking.k8s.io", "kind": "AdminNetworkPolicy"},
        "baselineadminnetworkpolicies.policy.networking.k8s.io": {"group": "policy.networking.k8s.io", "kind": "BaselineAdminNetworkPolicy"},
    }


class Dependencies(unittest.TestCase):
    def test_calico_bundled_admin_policies_are_accepted(self):
        quickstart.check_crds(installed_crds())

    def test_missing_sproozi_crd_fails(self):
        observed = installed_crds()
        del observed["agentruns.sproozi.com"]
        with self.assertRaisesRegex(RuntimeError, "all four"):
            quickstart.check_crds(observed)

    def test_external_sandbox_cannot_be_a_hidden_dependency(self):
        observed = installed_crds()
        observed["sandboxes.agents.x-k8s.io"] = {"group": "agents.x-k8s.io", "kind": "Sandbox"}
        with self.assertRaisesRegex(RuntimeError, "sandboxes.agents.x-k8s.io"):
            quickstart.check_crds(observed)

    def test_newer_host_kubectl_is_rejected_before_launch(self):
        def output(command):
            if command[0] == "kubectl":
                return json.dumps({"clientVersion": {"major": "1", "minor": "37"}})
            return "available"
        with patch.object(quickstart.shutil, "which", return_value="/tools/bin"), \
                patch.object(quickstart, "capture", side_effect=output):
            with self.assertRaisesRegex(RuntimeError, "pinned Kubernetes 1.33.1"):
                quickstart.doctor()


class Cleanup(unittest.TestCase):
    def test_foreign_directory_never_reaches_cluster_deletion(self):
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(quickstart, "EVIDENCE_ROOT", Path(temporary) / "proof"), \
                patch.object(quickstart.subprocess, "run") as run:
            with self.assertRaisesRegex(RuntimeError, "evidence directory"):
                quickstart.cleanup(Path(temporary))
            run.assert_not_called()

    def test_mismatched_cluster_owner_is_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            proof = root / "20261007t010203z-01234567"
            proof.mkdir()
            (proof / "owner.json").write_text(json.dumps({"path": "demo", "cluster": "real-dev-cluster"}))
            with patch.object(quickstart, "EVIDENCE_ROOT", root), \
                    patch.object(quickstart.subprocess, "run") as run:
                with self.assertRaisesRegex(RuntimeError, "recorded verification owner"):
                    quickstart.cleanup(proof)
                run.assert_not_called()

    def test_owned_cleanup_preserves_proof_and_other_cluster(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            run_id = "20261007t010203z-01234567"
            cluster = "sproozi-quickstart-" + run_id
            proof = root / run_id
            work = proof / "checkout"
            work.mkdir(parents=True)
            (work / "private-input").write_text("scratch")
            (proof / "commands.log").write_text("proof")
            (proof / "owner.json").write_text(json.dumps({"path": "demo", "cluster": cluster}))
            (proof / "result.json").write_text(json.dumps({"outcome": "passed"}))
            lock = root / "driver.lock"
            lock.mkdir()
            (lock / "owner").write_text(run_id)
            with patch.object(quickstart, "EVIDENCE_ROOT", root), \
                    patch.object(quickstart, "LOCK", lock), \
                    patch.object(quickstart, "capture", side_effect=["real-dev-cluster\n" + cluster, "real-dev-cluster"]), \
                    patch.object(quickstart.subprocess, "run") as run:
                quickstart.cleanup(proof)
                run.assert_called_once_with(["kind", "delete", "cluster", "--name", cluster], check=True, timeout=120)
            self.assertFalse(work.exists())
            self.assertFalse(lock.exists())
            self.assertEqual((proof / "commands.log").read_text(), "proof")
            result = json.loads((proof / "result.json").read_text())
            self.assertTrue(result["clusterRemoved"])
            self.assertTrue(result["scratchRemoved"])


if __name__ == "__main__":
    unittest.main()
