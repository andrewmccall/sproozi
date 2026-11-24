#!/usr/bin/env python3
"""Offline checks of the refresh scenario's API server configuration."""
import importlib.util
from pathlib import Path
import unittest

import yaml

HERE = Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("claude_remote_apiserver", HERE / "apiserver.py")
A = importlib.util.module_from_spec(spec)
spec.loader.exec_module(A)

# The shape kubeadm writes for Kind's control plane, shortened.
MANIFEST = """apiVersion: v1
kind: Pod
metadata:
  name: kube-apiserver
  namespace: kube-system
spec:
  containers:
  - command:
    - kube-apiserver
    - --advertise-address=172.18.0.3
    - --service-account-issuer=https://kubernetes.default.svc.cluster.local
    image: registry.k8s.io/kube-apiserver:v1.33.1
    name: kube-apiserver
    volumeMounts:
    - mountPath: /etc/kubernetes/pki
      name: k8s-certs
      readOnly: true
  hostNetwork: true
  volumes:
  - hostPath:
      path: /etc/kubernetes/pki
      type: DirectoryOrCreate
    name: k8s-certs
"""


class ConfigureTests(unittest.TestCase):
    def test_adds_the_token_cap_and_a_tokenreview_audit_log(self):
        pod = yaml.safe_load(A.configure(MANIFEST, 3607))
        container = pod["spec"]["containers"][0]
        self.assertEqual(container["command"][:3], yaml.safe_load(MANIFEST)["spec"]["containers"][0]["command"])
        self.assertIn("--service-account-max-token-expiration=3607s", container["command"])
        self.assertIn("--audit-log-path=" + A.LOG_DIRECTORY + "/audit.log", container["command"])
        self.assertIn("--audit-policy-file=" + A.POLICY_DIRECTORY + "/policy.yaml", container["command"])
        mounts = {m["name"]: m for m in container["volumeMounts"]}
        self.assertTrue(mounts["sproozi-audit-policy"]["readOnly"])
        self.assertEqual(mounts["k8s-certs"]["mountPath"], "/etc/kubernetes/pki")
        volumes = {v["name"]: v["hostPath"]["path"] for v in pod["spec"]["volumes"]}
        self.assertEqual(volumes["sproozi-audit-log"], A.LOG_DIRECTORY)

    def test_refuses_a_second_configuration_or_another_pod(self):
        with self.assertRaises(ValueError):
            A.configure(A.configure(MANIFEST, 3607), 3607)
        with self.assertRaises(ValueError):
            A.configure(MANIFEST.replace("- kube-apiserver\n", "- etcd\n"), 3607)

    def test_the_policy_records_only_tokenreviews(self):
        policy = yaml.safe_load(A.POLICY)
        self.assertEqual(policy["rules"][0]["resources"], [{"group": "authentication.k8s.io",
                                                            "resources": ["tokenreviews"]}])
        self.assertEqual(policy["rules"][-1], {"level": "None"})


if __name__ == "__main__":
    unittest.main()
