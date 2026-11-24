#!/usr/bin/env python3
"""Render the Claude remote-executor proof's cluster objects.

The objects are the shipped ones with only test inputs substituted: the
controller, gateway and sandbox namespace from config/, the Claude Code
executor example (examples/portable-workflows/kubernetes/claude) with its CLI
access, and, for this proof only, the deterministic Anthropic fixture provider
and a fixture sign-in in the example's login claim. Substituted: images, the
trust bundle, the gateway's Anthropic upstream, the executor's principal and an
offline Git snapshot in place of the GitHub repository. The example's template,
instructions, policy, roles, network policy and sign-in stay as shipped.

YAML on stdout holds no private key. run.sh creates the TLS and work-token
Secrets from private files.
"""
import argparse
import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import re
import ssl
import sys

import yaml

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[2]


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


KUBE = module("claude_remote_kube", ROOT / "hack/verify/session-mobility/kubernetes.py")
PROVIDER = module("claude_remote_provider", HERE / "provider.py")

SYSTEM, AGENTS = "sproozi-system", "sproozi-agents"
SESSIONS = "sproozi-claude-sessions"
FIXTURE = "sproozi-fixture"
EXAMPLE = ROOT / "examples/portable-workflows/kubernetes/claude"
PROVIDER_SERVICE = "claude-fixture." + FIXTURE + ".svc"
UPSTREAM = "https://" + PROVIDER_SERVICE + ":8443"
LOGIN_CLAIM = "sproozi-login-claude-code-default"
PRINCIPAL = "claude-remote-proof"
SNAPSHOT = "/state/source"
IMAGE = re.compile(r"[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}\Z")
RESTRICTED = {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True, "runAsNonRoot": True,
              "capabilities": {"drop": ["ALL"]}, "seccompProfile": {"type": "RuntimeDefault"}}
POD_SECURITY = {"runAsNonRoot": True, "runAsUser": 1000, "runAsGroup": 1000, "fsGroup": 1000,
                "seccompProfile": {"type": "RuntimeDefault"}}


def credentials():
    """Claude Code's own sign-in file, as /login leaves it, holding the
    fixture's placeholder OAuth token."""
    return json.dumps({"claudeAiOauth": {"accessToken": PROVIDER.OAUTH_TOKEN, "refreshToken": "sk-ant-ort01-fixture",
                                         "expiresAt": 4102444800000, "scopes": ["user:inference"],
                                         "subscriptionType": "max"}})


def gateway(documents, control_image, trust, auth):
    """The shared gateway, pointed at the fixture provider."""
    for doc in documents:
        if doc["kind"] != "Deployment":
            continue
        KUBE.configure_deployment(doc, control_image, trust)
        if doc["metadata"]["name"] != "sproozi-gateway":
            continue
        container = doc["spec"]["template"]["spec"]["containers"][0]
        env = [e for e in container["env"] if e["name"] not in ("OPENAI_API_KEY", "MODEL_AUTH_MODE")]
        env.append({"name": "ANTHROPIC_UPSTREAM_URL", "value": UPSTREAM})
        if auth == "sign-in":
            # The example's runtime signs in itself; the gateway forwards that
            # sign-in only in workload mode with no Anthropic key.
            env.append({"name": "MODEL_AUTH_MODE", "value": "workload"})
        else:
            env += [{"name": "MODEL_AUTH_MODE", "value": "api_key"},
                    {"name": "ANTHROPIC_API_KEY", "valueFrom": {"secretKeyRef": {
                        "name": "claude-remote-model-placeholder", "key": "anthropic-api-key"}}}]
        container["env"] = env
    return documents


def example(native_image, trust, auth):
    """The Claude Code executor example with the proof's substitutions."""
    documents = [d for d in KUBE.build(str(EXAMPLE.relative_to(ROOT))) if d]
    for doc in documents:
        kind = doc["kind"]
        if kind == "AgentRuntime":
            client = doc["spec"]["clientConfig"]
            client["trustBundleConfigMap"] = {"name": trust, "key": "ca-bundle.pem"}
            if auth != "sign-in":
                client.pop("signIn", None)
            for container in doc["spec"]["podTemplate"]["spec"]["containers"]:
                container["image"], container["imagePullPolicy"] = native_image, "IfNotPresent"
        if kind == "Deployment" and doc["metadata"]["name"] == "sproozi-work":
            pod = doc["spec"]["template"]["spec"]
            pod["containers"][0]["image"] = native_image
            pod["containers"][0]["imagePullPolicy"] = "IfNotPresent"
            # Only this offline fixture seeds a Git snapshot; the example
            # approves a GitHub URL, which the proof cluster cannot reach.
            pod["initContainers"] = [{"name": "seed", "image": native_image, "imagePullPolicy": "IfNotPresent",
                "command": ["/bin/sh", "-ec", KUBE.SEED],
                "securityContext": {k: v for k, v in RESTRICTED.items() if k != "runAsNonRoot"},
                "resources": {"requests": {"cpu": "100m", "memory": "64Mi"}, "limits": {"cpu": "500m", "memory": "256Mi"}},
                "volumeMounts": [{"name": "state", "mountPath": "/state"}, {"name": "tmp", "mountPath": "/tmp"}]}]
    return documents


def executor_profile():
    """The example's profile.json with the proof's principal and snapshot."""
    profile = json.loads((EXAMPLE / "profile.json").read_text())
    profile["principal"] = PRINCIPAL
    profile["repositories"] = {"sproozi": {"snapshot": SNAPSHOT}}
    return profile


def check_profile(profile, documents):
    """The executor's instructions digest must name the example template's."""
    templates = [d for d in documents if d["kind"] == "AgentTemplate"]
    if len(templates) != 1 or templates[0]["metadata"]["name"] != profile["templateName"]:
        raise ValueError("The example must hold exactly the executor's template")
    digest = hashlib.sha256(templates[0]["spec"]["instructions"].encode()).hexdigest()
    if digest != profile["instructionDigest"] or profile["harness"] != "claude-code":
        raise ValueError("The example's executor does not match its template")


def provider(native_image):
    """The fixture provider: its script, Deployment and Service."""
    script = (HERE / "provider.py").read_text()
    meta = lambda name: {"name": name, "namespace": FIXTURE}
    labels = {"app": "claude-fixture"}
    deployment = {"apiVersion": "apps/v1", "kind": "Deployment", "metadata": meta("claude-fixture"), "spec": {
        "replicas": 1, "selector": {"matchLabels": labels}, "template": {
            "metadata": {"labels": labels, "annotations": {
                "sproozi.test/provider": hashlib.sha256(script.encode()).hexdigest()[:16]}},
            "spec": {"automountServiceAccountToken": False, "securityContext": POD_SECURITY,
                     "containers": [{"name": "provider", "image": native_image, "imagePullPolicy": "IfNotPresent",
                                     "command": ["python3", "/fixture-code/provider.py"],
                                     "ports": [{"name": "https", "containerPort": 8443}],
                                     "readinessProbe": {"tcpSocket": {"port": "https"}},
                                     "securityContext": RESTRICTED,
                                     "resources": {"requests": {"cpu": "50m", "memory": "64Mi"},
                                                   "limits": {"cpu": "500m", "memory": "256Mi"}},
                                     "volumeMounts": [{"name": "code", "mountPath": "/fixture-code", "readOnly": True},
                                                      {"name": "tls", "mountPath": "/tls", "readOnly": True},
                                                      {"name": "tmp", "mountPath": "/tmp"}]}],
                     "volumes": [{"name": "code", "configMap": {"name": "claude-fixture-code"}},
                                 {"name": "tls", "secret": {"secretName": "claude-fixture-tls"}},
                                 {"name": "tmp", "emptyDir": {"sizeLimit": "16Mi"}}]}}}}
    return [{"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": FIXTURE}},
            {"apiVersion": "v1", "kind": "ConfigMap", "metadata": meta("claude-fixture-code"),
             "data": {"provider.py": script}},
            deployment,
            {"apiVersion": "v1", "kind": "Service", "metadata": meta("claude-fixture"),
             "spec": {"selector": labels, "ports": [{"name": "https", "port": 8443, "targetPort": "https"}]}}]


def login_seed(native_image):
    """The example runtime's login claim, holding a fixture sign-in, as if
    /login had run once in a session. The controller uses an existing claim."""
    claim = {"apiVersion": "v1", "kind": "PersistentVolumeClaim",
             "metadata": {"name": LOGIN_CLAIM, "namespace": AGENTS,
                          "labels": {"app.kubernetes.io/managed-by": "sproozi", "sproozi.com/login": "true"}},
             "spec": {"accessModes": ["ReadWriteOnce"], "resources": {"requests": {"storage": "16Mi"}}}}
    pod = {"apiVersion": "v1", "kind": "Pod", "metadata": {"name": "claude-login-seed", "namespace": AGENTS},
           "spec": {"restartPolicy": "Never", "automountServiceAccountToken": False, "securityContext": POD_SECURITY,
                    "containers": [{"name": "seed", "image": native_image, "imagePullPolicy": "IfNotPresent",
                                    "command": ["/bin/sh", "-ec",
                                                'umask 077; printf %s "$CREDENTIALS" > /login/.credentials.json'],
                                    "env": [{"name": "CREDENTIALS", "value": credentials()}],
                                    "securityContext": RESTRICTED,
                                    "resources": {"requests": {"cpu": "50m", "memory": "32Mi"},
                                                  "limits": {"cpu": "200m", "memory": "64Mi"}},
                                    "volumeMounts": [{"name": "login", "mountPath": "/login"}]}],
                    "volumes": [{"name": "login", "persistentVolumeClaim": {"claimName": LOGIN_CLAIM}}]}}
    return [claim, pod]


def lengthen(documents, profile, duration):
    """Raises the example's 30-minute cap, the executor's maxDuration and the
    policy's maxExecutionDuration, to duration, for a scenario that must
    outlive a gateway token."""
    if not re.fullmatch(r"[1-9][0-9]*[mh]", duration):
        raise ValueError("--max-duration must be a whole number of minutes or hours, such as 90m")
    policies = [d for d in documents if d["kind"] == "AgentPolicy"]
    if len(policies) != 1:
        raise ValueError("The example must hold exactly one AgentPolicy")
    policies[0]["spec"]["maxExecutionDuration"] = duration
    profile["maxDuration"] = duration


def render(control_image, native_image, ca, auth, max_duration=None):
    if not IMAGE.fullmatch(control_image) or not IMAGE.fullmatch(native_image):
        raise ValueError("Both images must be canonical repository@sha256 references")
    if len(ca.encode()) > 1 << 20 or not re.fullmatch(
            r"(?:\s*-----BEGIN CERTIFICATE-----\s+[A-Za-z0-9+/=\s]+-----END CERTIFICATE-----\s*)+", ca):
        raise ValueError("The CA file must hold only bounded PEM certificates")
    ssl.create_default_context(cadata=ca)
    trust = KUBE.trust_name(ca, "claude-remote-trust")
    documents = [d for d in KUBE.build("config/default")
                 if d and d["metadata"]["name"] in KUBE.KEEP.get(d["kind"], set())]
    documents.extend(d for d in KUBE.build("config/agent-rbac") if d)
    gateway(documents, control_image, trust, auth)
    documents += [KUBE.object_("ConfigMap", trust, ns, immutable=True, data={"ca-bundle.pem": ca})
                  for ns in (SYSTEM, AGENTS)]
    shipped = example(native_image, trust, auth)
    profile = executor_profile()
    check_profile(profile, shipped)
    if max_duration:
        lengthen(shipped, profile, max_duration)
    documents += shipped
    documents += [KUBE.object_("ConfigMap", "sproozi-work-profile", SESSIONS, immutable=True,
                               data={"profile.json": json.dumps(profile, indent=2)}),
                  KUBE.object_("ConfigMap", "sproozi-remote-executor", SESSIONS, immutable=True, data={"ca.crt": ca})]
    documents += [d for d in KUBE.build(str((EXAMPLE / "cli-access").relative_to(ROOT))) if d]
    documents += provider(native_image)
    if auth == "sign-in":
        documents += login_seed(native_image)
    return [copy.deepcopy(d) for d in documents], profile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--control-image", required=True)
    parser.add_argument("--native-image", required=True)
    parser.add_argument("--ca-file", type=Path, required=True)
    parser.add_argument("--auth", choices=["sign-in", "gateway-key"], default="sign-in")
    parser.add_argument("--max-duration", help="raise the example's executor and policy duration caps to this")
    args = parser.parse_args()
    documents, _ = render(args.control_image, args.native_image, args.ca_file.read_text(), args.auth,
                          args.max_duration)
    sys.stdout.write(yaml.safe_dump_all(documents, sort_keys=False))


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError, ssl.SSLError) as error:
        print("Claude remote proof render failed: " + str(error), file=sys.stderr)
        sys.exit(1)
