#!/usr/bin/env python3
"""Prove the release Kubernetes quickstart on a dedicated Kind cluster.

Builds the release images from this checkout, loads them into an owned Kind
cluster with an enforcing CNI, renders the release assets with the loaded
digests and installs them exactly as the guide does: CRDs, Established wait,
then the self-managed CA quickstart overlay. It then proves the provider-free
first check (permitted read, denied read, blocked direct path), leaf renewal
reaching the running gateway, routine CA rotation through new immutable trust
objects, and the certificate controller's RBAC limits. The report is written
to .local/verification/kubernetes-release/report.json. The cluster is deleted
unless --keep is given.
"""

import argparse
import base64
import importlib.util
import json
import os
from pathlib import Path
import secrets
import subprocess
import tarfile
import time

ROOT = Path(__file__).resolve().parents[2]
OUT = ROOT / ".local/verification/kubernetes-release"
# make verify-kubernetes-release passes the Makefile's pinned node image.
KIND_NODE_IMAGE = os.environ.get("KIND_NODE_IMAGE") or "kindest/node:v1.33.1@sha256:050072256b9a903bd914c0b2866828150cb229cea0efe5892e2b644d5dd3b34f"
SYSTEM, AGENTS = "sproozi-system", "sproozi-agents"
CERTIFICATES_SA = f"system:serviceaccount:{SYSTEM}:sproozi-certificates"


def run(*args, check=True, capture=True, env=None, timeout=900):
    result = subprocess.run(args, cwd=ROOT, text=True, capture_output=capture, check=False, env=env, timeout=timeout)
    if check and result.returncode != 0:
        raise RuntimeError(f"{' '.join(args)} failed: {result.stderr[-2000:] if capture else ''}")
    return result


class Cluster:
    def __init__(self, name, kubeconfig):
        self.name, self.kubeconfig = name, kubeconfig

    def kubectl(self, *args, check=True, timeout=300):
        return run("kubectl", "--kubeconfig", str(self.kubeconfig), *args, check=check, timeout=timeout)

    def json(self, *args):
        return json.loads(self.kubectl(*args, "-o", "json").stdout)

    def annotate(self, value):
        self.kubectl("annotate", "secret", "-n", SYSTEM, "sproozi-certificate-authority",
                     f"sproozi.com/rotation-request={value}", "--overwrite")

    def authority(self):
        return self.json("get", "secret", "-n", SYSTEM, "sproozi-certificate-authority")

    def serving_leaf(self):
        data = self.json("get", "secret", "-n", SYSTEM, "sproozi-gateway-tls").get("data", {})
        return base64.b64decode(data.get("tls.crt", ""))

    def restart_certificates(self):
        # Reconcile now instead of waiting for the controller's interval.
        self.kubectl("rollout", "restart", "-n", SYSTEM, "deployment/sproozi-certificates")
        self.kubectl("rollout", "status", "-n", SYSTEM, "deployment/sproozi-certificates", "--timeout=180s")


def wait(description, predicate, timeout=180, interval=3):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(interval)
    raise RuntimeError("timed out waiting for " + description)


def build_images():
    tags = {"native": "docker.io/sproozi-release/native:verify", "control": "docker.io/sproozi-release/control:verify"}
    run("docker", "buildx", "bake", "-f", "hack/release/docker-bake.hcl", "native", "control", "--load",
        "--set", f"native.tags={tags['native']}", "--set", f"control.tags={tags['control']}", capture=False, timeout=3600)
    return tags


def load_images(cluster, tags):
    spec = importlib.util.spec_from_file_location("kind_images", ROOT / "hack/verify/orchestration-kind/build.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    module.OUT = OUT
    os.environ["KIND_CLUSTER"] = cluster.name
    return {name: module.load(tag)["image"] for name, tag in tags.items()}


def capture_logs(cluster, name, store):
    """Follow the run's Pod while it exists; the Pod is removed after success."""
    def pod():
        status = cluster.json("get", "agentrun", "-n", SYSTEM, name).get("status", {})
        return status.get("identity", {}).get("sandboxName")
    sandbox = wait(f"{name} Pod name", pod, timeout=240)
    wait(f"{name} Pod", lambda: cluster.kubectl("get", "pod", "-n", AGENTS, sandbox, check=False).returncode == 0, timeout=240)
    wait(f"{name} output", lambda: "First check passed" in cluster.kubectl("logs", "-n", AGENTS, sandbox, "-c", "agent", check=False).stdout
         or "UNEXPECTED" in cluster.kubectl("logs", "-n", AGENTS, sandbox, "-c", "agent", check=False).stdout, timeout=300)
    store[name] = cluster.kubectl("logs", "-n", AGENTS, sandbox, "-c", "agent").stdout


def verify(args):
    OUT.mkdir(parents=True, exist_ok=True)
    report = {"started": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()), "passed": False, "steps": {}}
    revision = run("git", "rev-parse", "HEAD").stdout.strip()
    dirty = bool(run("git", "status", "--porcelain").stdout.strip())
    report["revision"], report["dirty"] = revision, dirty
    cluster = Cluster(args.cluster or "sproozi-release-" + secrets.token_hex(3), OUT / "kubeconfig")
    calico = OUT / "calico.yaml"
    run("bash", "hack/verify/fetch-calico.sh", str(calico))
    env = {**os.environ, "KIND_NODE_IMAGE": KIND_NODE_IMAGE, "SPROOZI_CNI_MANIFEST": str(calico), "SPROOZI_CNI_SELECTOR": "k8s-app=calico-node"}
    created = False
    try:
        tags = build_images()
        run("bash", "hack/kind-up.sh", cluster.name, str(cluster.kubeconfig), env=env, capture=False)
        created = True
        images = load_images(cluster, tags)
        report["images"] = images
        assets = OUT / "assets.tar.gz"
        run("python3", "hack/release/kubernetes.py", "--version", "0.0.0-verify", "--control-image", images["control"],
            "--native-image", images["native"], "--output", str(assets))
        with tarfile.open(assets) as archive:
            archive.extractall(OUT, filter="data")
        release = OUT / "sproozi-release"
        cluster.kubectl("apply", "--server-side", "--field-manager=sproozi-installer", "-f", str(release / "crds.yaml"))
        cluster.kubectl("wait", "--for=condition=Established", "--timeout=60s", "crd/agentruntimes.sproozi.com",
                        "crd/agentpolicies.sproozi.com", "crd/agenttemplates.sproozi.com", "crd/agentruns.sproozi.com",
                        "crd/budgetledgers.sproozi.com")
        cluster.kubectl("apply", "--server-side", "--field-manager=sproozi-installer", "-k", str(release / "quickstart-self-managed-ca"))
        for deployment in ["sproozi-certificates", "sproozi-controller-manager", "sproozi-gateway"]:
            cluster.kubectl("rollout", "status", "-n", SYSTEM, "deployment/" + deployment, "--timeout=300s", timeout=330)
        trust = cluster.json("get", "configmap", "-n", AGENTS, "sproozi-sandbox-trust")
        report["steps"]["bootstrap"] = {"servingSecretPopulated": bool(cluster.serving_leaf()), "trustImmutable": trust.get("immutable") is True,
                                        "trustHasPrivateKey": "PRIVATE KEY" in json.dumps(trust),
                                        "annotations": cluster.authority()["metadata"].get("annotations", {})}
        # Reapplying the overlay must not reset controller-owned certificate data.
        leaf = cluster.serving_leaf()
        cluster.kubectl("apply", "--server-side", "--field-manager=sproozi-installer", "-k", str(release / "quickstart-self-managed-ca"))
        report["steps"]["reapplyKeepsCertificates"] = cluster.serving_leaf() == leaf

        cluster.kubectl("apply", "-f", str(release / "first-check/resources.yaml"))
        logs = {}
        cluster.kubectl("create", "-f", str(release / "first-check/agentrun.yaml"))
        capture_logs(cluster, "first-check", logs)
        phase = wait("first-check phase", lambda: cluster.json("get", "agentrun", "-n", SYSTEM, "first-check").get("status", {}).get("phase") in ("Succeeded", "Failed", "TimedOut")
                     and cluster.json("get", "agentrun", "-n", SYSTEM, "first-check")["status"]["phase"], timeout=240)
        report["steps"]["firstCheck"] = {"phase": phase, "permitted": "PERMITTED: Pod read succeeded" in logs["first-check"],
                                         "deniedRead": "DENIED: Secret read" in logs["first-check"],
                                         "deniedDirect": "DENIED: direct API server path" in logs["first-check"],
                                         "deniedInternet": "DENIED: direct internet path" in logs["first-check"]}
        wait("first-check cleanup", lambda: not cluster.json("get", "pods", "-n", AGENTS)["items"], timeout=180)
        report["steps"]["firstCheck"]["podsRemoved"] = True

        # Renewal: a new leaf under the same CA reaches the running gateway.
        gateway_pod = cluster.json("get", "pods", "-n", SYSTEM, "-l", "app.kubernetes.io/component=shared-gateway")["items"][0]["metadata"]["name"]
        cluster.annotate("renew")
        cluster.restart_certificates()
        renewed = wait("renewed serving Secret", lambda: cluster.serving_leaf() != leaf and cluster.serving_leaf())
        reloaded = wait("gateway reload", lambda: "Reloaded gateway certificate" in cluster.kubectl("logs", "-n", SYSTEM, gateway_pod).stdout, timeout=240)
        report["steps"]["renewal"] = {"leafChanged": renewed != leaf, "gatewayReloadedWithoutRestart": bool(reloaded),
                                      "gatewayPodUnchanged": gateway_pod == cluster.json("get", "pods", "-n", SYSTEM, "-l", "app.kubernetes.io/component=shared-gateway")["items"][0]["metadata"]["name"]}

        # Routine rotation: prepare, switch runtimes to the combined bundle, switch, retire.
        cluster.annotate("prepare")
        cluster.restart_certificates()
        combined = cluster.json("get", "configmap", "-n", AGENTS, "sproozi-sandbox-trust-1-2")
        original = cluster.json("get", "configmap", "-n", AGENTS, "sproozi-sandbox-trust")
        cluster.kubectl("patch", "agentruntime", "-n", SYSTEM, "first-check", "--type=merge", "-p",
                        json.dumps({"spec": {"clientConfig": {"trustBundleConfigMap": {"name": "sproozi-sandbox-trust-1-2", "key": "ca-bundle.pem"}}}}))
        cluster.annotate("switch")
        cluster.restart_certificates()
        switched_leaf = wait("B-issued leaf", lambda: cluster.serving_leaf() not in (leaf, renewed) and cluster.serving_leaf())
        wait("gateway reload after switch", lambda: cluster.kubectl("logs", "-n", SYSTEM, gateway_pod).stdout.count("Reloaded gateway certificate") >= 2, timeout=240)
        cluster.kubectl("delete", "agentrun", "-n", SYSTEM, "first-check")
        cluster.kubectl("create", "-f", str(release / "first-check/agentrun.yaml"))
        capture_logs(cluster, "first-check", logs)
        switched = "First check passed" in logs["first-check"]
        cluster.annotate("retire")
        cluster.restart_certificates()
        retired = cluster.json("get", "configmap", "-n", AGENTS, "sproozi-sandbox-trust-2")
        cluster.kubectl("patch", "agentruntime", "-n", SYSTEM, "first-check", "--type=merge", "-p",
                        json.dumps({"spec": {"clientConfig": {"trustBundleConfigMap": {"name": "sproozi-sandbox-trust-2", "key": "ca-bundle.pem"}}}}))
        wait("previous run to finish", lambda: cluster.json("get", "agentrun", "-n", SYSTEM, "first-check").get("status", {}).get("phase") in ("Succeeded", "Failed", "TimedOut"), timeout=240)
        cluster.kubectl("delete", "agentrun", "-n", SYSTEM, "first-check")
        cluster.kubectl("create", "-f", str(release / "first-check/agentrun.yaml"))
        capture_logs(cluster, "first-check", logs)
        authority = cluster.authority()
        report["steps"]["rotation"] = {"combinedImmutable": combined.get("immutable") is True,
                                       "originalUnchanged": original["data"] == cluster.json("get", "configmap", "-n", AGENTS, "sproozi-sandbox-trust")["data"],
                                       "switchedLeafChanged": switched_leaf not in (leaf, renewed), "runPassedOnCombinedTrust": switched,
                                       "retiredImmutable": retired.get("immutable") is True, "runPassedOnNewTrust": "First check passed" in logs["first-check"],
                                       "oldKeyDestroyed": "ca-1.key" not in authority.get("data", {}),
                                       "stage": authority["metadata"]["annotations"].get("sproozi.com/rotation-stage")}

        def can(verb, resource, namespace, name=None):
            target = f"{resource}/{name}" if name else resource
            return cluster.kubectl("auth", "can-i", verb, target, "-n", namespace, "--as", CERTIFICATES_SA, check=False).stdout.strip() == "yes"
        report["steps"]["leastPrivilege"] = {
            "updateOwnSecret": can("update", "secrets", SYSTEM, "sproozi-gateway-tls"),
            "readOtherSecret": can("get", "secrets", SYSTEM, "model-gateway-credentials"),
            "listSecrets": can("list", "secrets", SYSTEM),
            "readAgentSecrets": can("get", "secrets", AGENTS),
            "updatePolicies": can("update", "agentpolicies.sproozi.com", SYSTEM),
            "patchTrust": can("patch", "configmaps", AGENTS, "sproozi-sandbox-trust"),
            "clusterAdmin": can("*", "*", SYSTEM)}
        lp = report["steps"]["leastPrivilege"]
        steps = report["steps"]
        report["passed"] = all([
            steps["bootstrap"]["servingSecretPopulated"], steps["bootstrap"]["trustImmutable"], not steps["bootstrap"]["trustHasPrivateKey"],
            steps["reapplyKeepsCertificates"], steps["firstCheck"]["phase"] == "Succeeded", steps["firstCheck"]["permitted"],
            steps["firstCheck"]["deniedRead"], steps["firstCheck"]["deniedDirect"], steps["firstCheck"]["deniedInternet"], all(steps["renewal"].values()),
            all(v for k, v in steps["rotation"].items() if k != "stage"), steps["rotation"]["stage"] == "none",
            lp["updateOwnSecret"], not any(v for k, v in lp.items() if k != "updateOwnSecret")])
        report["logs"] = logs
    finally:
        if created and not report["passed"]:
            # Keep enough controller and run state to diagnose a failed proof.
            diagnostics = OUT / "diagnostics"
            diagnostics.mkdir(exist_ok=True)
            for name, argv in {"controller.log": ("logs", "-n", SYSTEM, "deployment/sproozi-controller-manager", "--all-containers"),
                               "agentruns.yaml": ("get", "agentruns", "-A", "-o", "yaml"),
                               "pods.yaml": ("get", "pods", "-n", AGENTS, "-o", "yaml"),
                               "events.txt": ("get", "events", "-A", "--sort-by=.lastTimestamp")}.items():
                (diagnostics / name).write_text(cluster.kubectl(*argv, check=False).stdout)
            report["diagnostics"] = str(diagnostics)
        report["finished"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        (OUT / "report.json").write_text(json.dumps(report, indent=2) + "\n")
        if created and not args.keep:
            run("kind", "delete", "cluster", "--name", cluster.name, check=False)
    print(json.dumps({"passed": report["passed"], "report": str(OUT / "report.json")}))
    if not report["passed"]:
        raise SystemExit(1)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cluster", help="dedicated Kind cluster name to create; a random sproozi-release-* name by default")
    parser.add_argument("--keep", action="store_true", help="keep the cluster for inspection")
    verify(parser.parse_args())
