"""Offline whole-render trust rotation checks; no cluster or provider requests."""
import contextlib
import copy
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import yaml

SPEC = importlib.util.spec_from_file_location("mobility_claude_render", Path(__file__).with_name("claude.py"))
CLAUDE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CLAUDE)
KUBE = CLAUDE.KUBE


class TrustRotationTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temporary = tempfile.TemporaryDirectory()
        cls.root = Path(cls.temporary.name)
        cls.certificates = []
        for index in range(2):
            certificate, key = cls.root / f"ca-{index}.pem", cls.root / f"ca-{index}.key"
            CLAUDE.HOST.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
                             "-subj", f"/CN=fixture-ca-{index}", "-addext", "basicConstraints=critical,CA:TRUE",
                             "-out", str(certificate), "-keyout", str(key)], cls.root)
            cls.certificates.append((certificate, key))

    @classmethod
    def tearDownClass(cls):
        cls.temporary.cleanup()

    def render(self, harness, certificate, key):
        with tempfile.TemporaryDirectory(dir=self.root) as directory:
            out = Path(directory)
            instructions = CLAUDE.INSTRUCTIONS if harness == "claude-code" else CLAUDE.HOST.INSTRUCTIONS
            hints = {"harness": harness,
                     "providerToken": CLAUDE.TOKEN if harness == "claude-code" else CLAUDE.HOST.TOKEN,
                     "providerURLFromKind": CLAUDE.UPSTREAM if harness == "claude-code" else KUBE.UPSTREAM,
                     "model": CLAUDE.MODEL if harness == "claude-code" else "gpt-6.1-sol", "nativeOptions": [],
                     "instructions": instructions, "instructionDigest": hashlib.sha256(instructions.encode()).hexdigest(),
                     "allowances": {"model.inference": {"units": {"bounded": True, "value": 200000},
                                                          "costMicros": {"bounded": False, "value": 0}}},
                     "maxDuration": "20m", "maxCheckpointBytes": 8388608}
            path = out / "hints.json"
            path.write_text(json.dumps(hints))
            image = "registry.example/fixture@sha256:" + "a" * 64
            args = SimpleNamespace(control_image=image, native_image=image, hints=path, ca_file=certificate,
                                   gateway_cert=certificate, gateway_key=key, profile_config_output=out / "profile.json")
            gateway = {"kind": "Deployment", "metadata": {"name": "sproozi-gateway"},
                       "spec": {"template": {"spec": {"containers": [{"name": "gateway", "volumeMounts": []}],
                                                       "volumes": []}}}}
            # Base deployment rendering is unrelated to CA selection. Exercise
            # both public render entrypoints and inspect their complete YAML.
            def build(directory):
                return [copy.deepcopy(gateway)] if directory == "config/default" else []
            output = io.StringIO()
            with patch.object(KUBE, "build", side_effect=build), contextlib.redirect_stdout(output):
                if harness == "claude-code":
                    CLAUDE.render(args)
                else:
                    flags = ["render", "--control-image", image, "--native-image", image,
                             "--hints", str(path), "--ca-file", str(certificate), "--gateway-cert", str(certificate),
                             "--gateway-key", str(key), "--profile-config-output", str(args.profile_config_output)]
                    with patch.object(sys, "argv", flags):
                        KUBE.main()
            self.assertTrue(args.profile_config_output.is_file())
            return list(yaml.safe_load_all(output.getvalue()))

    def assert_bindings(self, documents, certificate, harness):
        ca = certificate.read_text()
        prefix = "claude-mobility-fixture-trust" if harness == "claude-code" else "mobility-fixture-trust"
        expected = prefix + "-" + hashlib.sha256(ca.encode()).hexdigest()[:16]
        maps = [doc for doc in documents if doc["kind"] == "ConfigMap" and "ca-bundle.pem" in doc.get("data", {})]
        self.assertEqual({(doc["metadata"]["namespace"], doc["metadata"]["name"]) for doc in maps},
                         {("sproozi-system", expected), ("sproozi-agents", expected)})
        self.assertEqual(len(maps), 2)
        self.assertTrue(all(doc["immutable"] and doc["data"]["ca-bundle.pem"] == ca for doc in maps))
        gateway = next(doc for doc in documents if doc["kind"] == "Deployment")
        volumes = gateway["spec"]["template"]["spec"]["volumes"]
        self.assertEqual(next(volume for volume in volumes if volume["name"] == "fixture-ca")["configMap"]["name"], expected)
        runtime = next(doc for doc in documents if doc["kind"] == "AgentRuntime")
        self.assertEqual(runtime["spec"]["clientConfig"]["trustBundleConfigMap"],
                         {"name": expected, "key": "ca-bundle.pem"})
        self.assertEqual(runtime["spec"]["clientConfig"]["harness"], harness)
        return expected

    def assert_executor(self, documents, certificate, harness):
        """The executor is the public example, reached through a context."""
        named = {(doc["kind"], doc["metadata"]["name"]): doc for doc in documents}
        service = named[("Service", "sproozi-work")]
        self.assertEqual(service["metadata"]["labels"]["sproozi.com/remote-executor"], "true")
        self.assertEqual(named[("ConfigMap", "sproozi-remote-executor")]["data"]["ca.crt"], certificate.read_text())
        profile = json.loads(named[("ConfigMap", "sproozi-work-profile")]["data"]["profile.json"])
        self.assertEqual(profile["harness"], harness)
        # The executor and its managed runs live apart from the gateway's
        # configuration in sproozi-system, as in the public example.
        self.assertEqual(profile["namespace"], "sproozi-sessions")
        for kind, name in [("Service", "sproozi-work"), ("Deployment", "sproozi-work"),
                           ("ConfigMap", "sproozi-work-profile"), ("ConfigMap", "sproozi-remote-executor"),
                           ("ServiceAccount", "sproozi-work")]:
            self.assertEqual(named[(kind, name)]["metadata"]["namespace"], "sproozi-sessions", (kind, name))
        for kind in ("AgentRuntime", "AgentPolicy", "AgentTemplate"):
            managed = [doc for doc in documents if doc["kind"] == kind]
            self.assertEqual([doc["metadata"]["namespace"] for doc in managed], ["sproozi-sessions"], kind)
        self.assertNotIn("tokenFile", profile)
        self.assertTrue(profile["certificateFile"].startswith("/var/run/sproozi-work/"))
        self.assertIn(("ClusterRoleBinding", "sproozi-work-auth-delegator"), named)
        image = named[("Deployment", "sproozi-work")]["spec"]["template"]["spec"]["containers"][0]["image"]
        self.assertTrue(image.startswith("registry.example/fixture@sha256:"))

    def test_rotation_selects_new_immutable_objects_and_consistent_references(self):
        for harness in ("codex", "claude-code"):
            with self.subTest(harness=harness):
                names = []
                for certificate, key in self.certificates:
                    documents = self.render(harness, certificate, key)
                    names.append(self.assert_bindings(documents, certificate, harness))
                    self.assert_executor(documents, certificate, harness)
                self.assertNotEqual(names[0], names[1])
                certificate, key = self.certificates[0]
                repeated = self.assert_bindings(self.render(harness, certificate, key), certificate, harness)
                self.assertEqual(repeated, names[0])
        self.assertEqual(KUBE.TRUST, "mobility-fixture-trust")


if __name__ == "__main__":
    unittest.main()
