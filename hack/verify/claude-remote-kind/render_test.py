#!/usr/bin/env python3
"""Offline checks that the proof renders the shipped Claude Code executor
example with only test inputs substituted; no cluster or kustomize."""
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import yaml

HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location("claude_remote_render", HERE / "render.py")
R = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(R)
IMAGE = "registry.example/fixture@sha256:" + "a" * 64


def build(directory):
    """config/ stands in as a bare gateway; the example is its files, which
    its kustomization only lists."""
    if directory == "config/default":
        return [{"kind": "Deployment", "metadata": {"name": "sproozi-gateway", "namespace": R.SYSTEM},
                 "spec": {"template": {"spec": {"containers": [{"name": "gateway", "volumeMounts": []}],
                                                "volumes": []}}}}]
    if directory == str(R.EXAMPLE.relative_to(R.ROOT)):
        return [d for name in ("owner.yaml", "runtime.yaml")
                for d in yaml.safe_load_all((R.EXAMPLE / name).read_text()) if d]
    return []


class RenderTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temporary = tempfile.TemporaryDirectory()
        root = Path(cls.temporary.name)
        subprocess.run(["openssl", "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1", "-subj", "/CN=fixture",
                        "-addext", "basicConstraints=critical,CA:TRUE", "-out", str(root / "ca.pem"),
                        "-keyout", str(root / "ca.key")], check=True, capture_output=True, timeout=60)
        cls.ca = (root / "ca.pem").read_text()

    @classmethod
    def tearDownClass(cls):
        cls.temporary.cleanup()

    def render(self, auth):
        with patch.object(R.KUBE, "build", side_effect=build):
            documents, profile = R.render(IMAGE, IMAGE, self.ca, auth)
        yaml.safe_dump_all(documents)
        return documents, profile

    def find(self, documents, kind, name=None):
        found = [d for d in documents if d["kind"] == kind and (name is None or d["metadata"]["name"] == name)]
        self.assertEqual(len(found), 1, (kind, name))
        return found[0]

    def test_the_shipped_example_keeps_its_sign_in_template_and_network_policy(self):
        documents, profile = self.render("sign-in")
        runtime = self.find(documents, "AgentRuntime")
        self.assertEqual(runtime["metadata"]["namespace"], R.SESSIONS)
        self.assertIn("claims", runtime["spec"]["clientConfig"]["signIn"])
        self.assertTrue(runtime["spec"]["clientConfig"]["trustBundleConfigMap"]["name"].startswith("claude-remote-trust-"))
        self.assertTrue(all(c["image"] == IMAGE for c in runtime["spec"]["podTemplate"]["spec"]["containers"]))
        policy = self.find(documents, "NetworkPolicy", "sproozi-work-private")
        self.assertEqual(policy["spec"]["ingress"][0]["ports"][0]["port"], 8443)
        self.assertEqual(profile["harness"], "claude-code")
        self.assertEqual(profile["repositories"], {"sproozi": {"snapshot": R.SNAPSHOT}})
        self.assertEqual(profile["principal"], R.PRINCIPAL)
        stored = json.loads(self.find(documents, "ConfigMap", "sproozi-work-profile")["data"]["profile.json"])
        self.assertEqual(stored, profile)
        executor = self.find(documents, "Deployment", "sproozi-work")["spec"]["template"]["spec"]
        self.assertEqual(executor["containers"][0]["image"], IMAGE)
        self.assertEqual(executor["initContainers"][0]["name"], "seed")
        claim = self.find(documents, "PersistentVolumeClaim", R.LOGIN_CLAIM)
        self.assertEqual(claim["metadata"]["namespace"], R.AGENTS)
        seed = self.find(documents, "Pod", "claude-login-seed")
        sign_in = json.loads(seed["spec"]["containers"][0]["env"][0]["value"])
        self.assertEqual(sign_in["claudeAiOauth"]["accessToken"], R.PROVIDER.OAUTH_TOKEN)
        env = {e["name"]: e.get("value") for e in self.find(documents, "Deployment", "sproozi-gateway")[
            "spec"]["template"]["spec"]["containers"][0]["env"]}
        self.assertEqual(env["MODEL_AUTH_MODE"], "workload")
        self.assertEqual(env["ANTHROPIC_UPSTREAM_URL"], R.UPSTREAM)
        self.assertNotIn("ANTHROPIC_API_KEY", env)
        self.assertNotIn("OPENAI_API_KEY", env)
        provider = self.find(documents, "ConfigMap", "claude-fixture-code")
        self.assertEqual(provider["data"]["provider.py"], (HERE / "provider.py").read_text())

    def test_gateway_key_mode_removes_the_sign_in(self):
        documents, _ = self.render("gateway-key")
        runtime = self.find(documents, "AgentRuntime")
        self.assertNotIn("signIn", runtime["spec"]["clientConfig"])
        self.assertFalse([d for d in documents if d["kind"] == "Pod"])
        env = {e["name"]: e for e in self.find(documents, "Deployment", "sproozi-gateway")[
            "spec"]["template"]["spec"]["containers"][0]["env"]}
        self.assertEqual(env["MODEL_AUTH_MODE"]["value"], "api_key")
        self.assertEqual(env["ANTHROPIC_API_KEY"]["valueFrom"]["secretKeyRef"]["name"], "claude-remote-model-placeholder")

    def test_the_refresh_scenario_raises_only_the_duration_caps(self):
        shipped, shipped_profile = self.render("sign-in")
        with patch.object(R.KUBE, "build", side_effect=build):
            documents, profile = R.render(IMAGE, IMAGE, self.ca, "sign-in", "90m")
        self.assertEqual(self.find(documents, "AgentPolicy")["spec"]["maxExecutionDuration"], "90m")
        self.assertEqual(profile["maxDuration"], "90m")
        self.assertEqual(json.loads(self.find(documents, "ConfigMap", "sproozi-work-profile")["data"]["profile.json"]),
                         profile)
        self.assertEqual({**profile, "maxDuration": shipped_profile["maxDuration"]}, shipped_profile)
        self.find(shipped, "AgentPolicy")["spec"]["maxExecutionDuration"] = "90m"
        self.find(shipped, "ConfigMap", "sproozi-work-profile")["data"]["profile.json"] = \
            self.find(documents, "ConfigMap", "sproozi-work-profile")["data"]["profile.json"]
        self.assertEqual(documents, shipped)
        with patch.object(R.KUBE, "build", side_effect=build), self.assertRaises(ValueError):
            R.render(IMAGE, IMAGE, self.ca, "sign-in", "none")

    def test_a_changed_example_template_is_refused(self):
        documents, profile = self.render("sign-in")
        template = self.find(documents, "AgentTemplate")
        template["spec"]["instructions"] += " Changed."
        with self.assertRaises(ValueError):
            R.check_profile(profile, documents)


if __name__ == "__main__":
    unittest.main()
