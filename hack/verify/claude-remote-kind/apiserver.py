#!/usr/bin/env python3
"""Configure the proof cluster's API server for the refresh scenario.

Reads the kube-apiserver static Pod manifest on stdin and writes it with a
ServiceAccount token cap and an audit log of TokenReviews, so the scenario can
see when the gateway's TokenReview of each sandbox token was decided and which
token (its jti) it was. Only the Kind proof cluster's manifest is changed;
nothing in Sproozi is.
"""
import sys

import yaml

POLICY_DIRECTORY = "/etc/kubernetes/sproozi-audit"
LOG_DIRECTORY = "/var/log/kubernetes/sproozi-audit"
POLICY = """apiVersion: audit.k8s.io/v1
kind: Policy
omitStages: [RequestReceived]
rules:
- level: RequestResponse
  resources:
  - group: authentication.k8s.io
    resources: [tokenreviews]
- level: None
"""


def configure(manifest, token_seconds):
    pod = yaml.safe_load(manifest)
    containers = pod["spec"]["containers"]
    if len(containers) != 1 or containers[0]["command"][0] != "kube-apiserver":
        raise ValueError("Not the kube-apiserver static Pod")
    container = containers[0]
    flags = ["--service-account-max-token-expiration=%ds" % token_seconds,
             "--audit-policy-file=" + POLICY_DIRECTORY + "/policy.yaml",
             "--audit-log-path=" + LOG_DIRECTORY + "/audit.log"]
    names = {flag.split("=")[0] for flag in flags}
    if any(arg.split("=")[0] in names for arg in container["command"]):
        raise ValueError("The API server already sets a token cap or an audit log")
    container["command"] += flags
    container.setdefault("volumeMounts", []).extend([
        {"name": "sproozi-audit-policy", "mountPath": POLICY_DIRECTORY, "readOnly": True},
        {"name": "sproozi-audit-log", "mountPath": LOG_DIRECTORY}])
    pod["spec"].setdefault("volumes", []).extend([
        {"name": "sproozi-audit-policy", "hostPath": {"path": POLICY_DIRECTORY, "type": "Directory"}},
        {"name": "sproozi-audit-log", "hostPath": {"path": LOG_DIRECTORY, "type": "DirectoryOrCreate"}}])
    return yaml.safe_dump(pod, sort_keys=False)


def main():
    if sys.argv[1:] == ["policy"]:
        sys.stdout.write(POLICY)
    elif len(sys.argv) == 3 and sys.argv[1] == "manifest" and sys.argv[2].isdigit():
        sys.stdout.write(configure(sys.stdin.read(), int(sys.argv[2])))
    else:
        raise SystemExit("usage: apiserver.py policy | apiserver.py manifest TOKEN_SECONDS < kube-apiserver.yaml")


if __name__ == "__main__":
    main()
