#!/usr/bin/env python3
"""Render the isolated native session proof's controller, shared gateway and remote executor.

YAML on stdout contains only public fixture configuration. Create the gateway
and executor TLS Secrets separately; administrator KUBECONFIG stays on the host.
The executor is the public retained-work example, reached through a kubeconfig
context as the sproozi-cli ServiceAccount. This renders infrastructure, not a
completed mobility proof.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import ssl
import subprocess
import sys
import tempfile

import yaml

ROOT = Path(__file__).resolve().parents[3]
SYSTEM = "sproozi-system"
AGENTS = "sproozi-agents"
GATEWAY = "sproozi-gateway.sproozi-system.svc"
TRUST = "mobility-fixture-trust"
TEMPLATE = "mobility-native"
EXECUTOR = "sproozi-work"
CREDENTIALS = "/var/run/sproozi-work"
UPSTREAM = "https://192.168.65.254:18765"
IMAGE = re.compile(r"[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}\Z")
KEEP = {
    "Namespace": {SYSTEM},
    "ServiceAccount": {"sproozi-controller-manager", "sproozi-shared-gateway"},
    "Role": {"sproozi-leader-election-role", "sproozi-shared-gateway-budget"},
    "RoleBinding": {"sproozi-leader-election-rolebinding", "sproozi-shared-gateway-budget"},
    "ClusterRole": {"sproozi-controller-manager-role", "sproozi-shared-gateway"},
    "ClusterRoleBinding": {"sproozi-controller-manager-rolebinding", "sproozi-shared-gateway"},
    "ConfigMap": {"sproozi-model-gateway-pricing", "sproozi-mcp-servers"},
    "Service": {"sproozi-gateway"},
    "Deployment": {"sproozi-controller-manager", "sproozi-gateway"},
    "NetworkPolicy": {"sproozi-default-deny-ingress", "sproozi-allow-agent-traffic"},
}


def object_(kind, name, namespace=SYSTEM, **fields):
    api = "sproozi.com/v1alpha1" if kind.startswith("Agent") else "v1"
    return {"apiVersion": api, "kind": kind,
            "metadata": {"name": name, "namespace": namespace}, **fields}


def build(directory):
    output = subprocess.run([str(ROOT / "bin/kustomize-v5.8.1"), "build", str(ROOT / directory)],
                            check=True, capture_output=True, text=True, timeout=30).stdout
    return list(yaml.safe_load_all(output))


def hints_from(path):
    if path.stat().st_size > 64 << 10:
        raise ValueError("Profile hints exceed the bound")
    hints = json.loads(path.read_text())
    instructions = hints["instructions"]
    if not isinstance(instructions, str) or not instructions or len(instructions.encode()) > 32768:
        raise ValueError("Approved instructions are invalid")
    if hashlib.sha256(instructions.encode()).hexdigest() != hints["instructionDigest"]:
        raise ValueError("Instruction digest does not match the exact approved bytes")
    allowance = hints["allowances"]
    if set(allowance) != {"model.inference"}:
        raise ValueError("Fixture grants only model.inference")
    for name in ("units", "costMicros"):
        limit = allowance["model.inference"][name]
        value = limit["value"]
        if type(limit["bounded"]) is not bool or type(value) is not int or value < 0 or value > (1 << 63) - 1:
            raise ValueError("Fixture allowance is invalid")
        if limit["bounded"] and value == 0 or not limit["bounded"] and value != 0:
            raise ValueError("Fixture allowance is exhausted or malformed")
    if not allowance["model.inference"]["units"]["bounded"]:
        raise ValueError("Fixture model units must be finite")
    if hints.get("providerToken") != "tui-placeholder" or hints.get("providerURLFromKind") != UPSTREAM:
        raise ValueError("Only the public placeholder HTTPS fixture upstream is supported")
    if not re.fullmatch(r"[1-9][0-9]*[smh]", hints["maxDuration"]):
        raise ValueError("Fixture duration must be a positive whole seconds, minutes or hours value")
    number, unit = int(hints["maxDuration"][:-1]), hints["maxDuration"][-1]
    if number * {"s": 1, "m": 60, "h": 3600}[unit] > 3600:
        raise ValueError("Fixture duration exceeds policy maximum")
    if type(hints["maxCheckpointBytes"]) is not int or not 0 < hints["maxCheckpointBytes"] <= 256 << 20:
        raise ValueError("Checkpoint bound is invalid")
    options = hints["nativeOptions"]
    if not isinstance(options, list) or len(options) > 32 or any(
            not isinstance(value, str) or "=" not in value or len(value) > 4096 or "\0" in value
            or value.split("=", 1)[0] in {"developer_instructions", "check_for_update_on_startup"}
            for value in options):
        raise ValueError("Native launch options are invalid")
    return hints


def trust_name(ca, prefix=TRUST):
    # Immutable ConfigMaps are cached by kubelet. Recreating their old name can
    # retain old roots even in a new Pod; changed bytes must select a new name.
    return prefix + "-" + hashlib.sha256(ca.encode()).hexdigest()[:16]


def configure_deployment(doc, image, trust):
    pod = doc["spec"]["template"]["spec"]
    container = pod["containers"][0]
    container["image"] = image
    container["imagePullPolicy"] = "IfNotPresent"
    if doc["metadata"]["name"] != "sproozi-gateway":
        return
    environment = {
        "SPROOZI_ENABLED_CAPABILITIES": "model.inference",
        "SPROOZI_GATEWAY_CERT_FILE": "/var/run/sproozi-gateway/tls.crt",
        "SPROOZI_GATEWAY_KEY_FILE": "/var/run/sproozi-gateway/tls.key",
        "SPROOZI_GATEWAY_HOST": GATEWAY,
        "AGENT_NAMESPACE": AGENTS, "BUDGET_NAMESPACE": SYSTEM,
        "MODEL_AUTH_MODE": "api_key", "OPENAI_UPSTREAM_URL": UPSTREAM,
        "MODEL_PRICING_PATH": "/etc/sproozi/model/pricing.json",
        "MCP_SERVERS_PATH": "/etc/sproozi/mcp/servers.json",
        "SSL_CERT_FILE": "/etc/sproozi/fixture/ca-bundle.pem",
    }
    container["env"] = [{"name": key, "value": value} for key, value in environment.items()]
    container["env"].append({"name": "OPENAI_API_KEY", "valueFrom": {"secretKeyRef": {
        "name": "mobility-model-placeholder", "key": "openai-api-key"}}})
    mounts = {"gateway-tls", "model-pricing", "mcp-servers"}
    container["volumeMounts"] = [value for value in container["volumeMounts"] if value["name"] in mounts]
    container["volumeMounts"].append({"name": "fixture-ca", "mountPath": "/etc/sproozi/fixture", "readOnly": True})
    pod["volumes"] = [value for value in pod["volumes"] if value["name"] in mounts]
    pod["volumes"].append({"name": "fixture-ca", "configMap": {"name": trust}})


def fixture_resources(hints, image, ca, trust):
    security = {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                "runAsNonRoot": True, "capabilities": {"drop": ["ALL"]},
                "seccompProfile": {"type": "RuntimeDefault"}}
    containers = []
    for name, memory, cpu, command in [
            ("agent", "1Gi", "1", ["/usr/local/bin/native-session"]),
            ("checkpoint", "512Mi", "500m", ["/usr/local/bin/checkpoint"])]:
        containers.append({"name": name, "image": image, "imagePullPolicy": "IfNotPresent",
                           "command": command, "securityContext": security,
                           "resources": {"requests": {"cpu": "100m", "memory": memory,
                                                      "ephemeral-storage": "64Mi"},
                                         "limits": {"cpu": cpu, "memory": memory,
                                                    "ephemeral-storage": "256Mi"}}})
    model = hints.get("model", "gpt-6.1-sol")
    runtime = object_("AgentRuntime", TEMPLATE, spec={
        "podTemplate": {"spec": {"automountServiceAccountToken": False,
                                  "securityContext": {"runAsNonRoot": True, "runAsUser": 1000,
                                                      "runAsGroup": 1000, "fsGroup": 1000,
                                                      "seccompProfile": {"type": "RuntimeDefault"}},
                                  "containers": containers}},
        "workloadContainers": ["agent"],
        "clientConfig": {"harness": "codex", "nativeSession": {"model": model, "options": hints["nativeOptions"]},
                         "trustBundleConfigMap": {"name": trust, "key": "ca-bundle.pem"}},
        "ephemeralWorkspace": {"sizeLimit": "256Mi"},
        "gatewayEndpoint": "https://" + GATEWAY + ":8443"})
    allowance = hints["allowances"]["model.inference"]
    budget = {"maxUnits": allowance["units"]["value"], "maxCostMicros": allowance["costMicros"]["value"]}
    policy = object_("AgentPolicy", TEMPLATE, spec={
        # Required schema fields carry no authority without the corresponding capability.
        "allowedCapabilities": ["model.inference"], "kubernetesRead": {"namespaces": [SYSTEM], "resources": ["pods"]},
        "githubPullRequest": {"repositories": [], "allowedBaseBranches": []}, "egressProfiles": [],
        "budgets": {"model.inference": budget},
        "resourceBounds": {"max": {"cpu": "1500m", "memory": "1536Mi", "ephemeral-storage": "512Mi"}},
        "maxExecutionDuration": hints["maxDuration"], "retentionTTL": "1h"})
    template = object_("AgentTemplate", TEMPLATE, spec={
        "runtimeRef": {"name": TEMPLATE}, "policyRef": {"name": TEMPLATE},
        "instructions": hints["instructions"], "egressProfiles": []})
    template["metadata"]["annotations"] = {"sessions.sproozi.com/managed": "true"}
    return [object_("ConfigMap", trust, ns, immutable=True, data={"ca-bundle.pem": ca}) for ns in (SYSTEM, AGENTS)] + [
        object_("Secret", "mobility-model-placeholder", type="Opaque", stringData={"openai-api-key": hints["providerToken"]}),
        runtime, policy, template]


def executor_profile(hints, principal="mobility-proof", harness="codex", template=TEMPLATE, directory="/state/profile"):
    """The remote executor's configuration as its Pod reads it."""
    return {"listen": "0.0.0.0:8443", "certificateFile": CREDENTIALS + "/tls.crt", "keyFile": CREDENTIALS + "/tls.key",
            "principal": principal, "harness": harness, "placementName": "remote",
            "namespace": SYSTEM, "templateName": template, "directory": directory,
            "instructionDigest": hints["instructionDigest"], "allowances": hints["allowances"],
            "maxDuration": hints["maxDuration"], "maxCheckpointBytes": hints["maxCheckpointBytes"]}


SEED = ("mkdir -p /state/source; if ! test -d /state/source/.git; then git init --quiet --template= /state/source; "
        "printf 'Portable workflow fixture\\n' > /state/source/note.txt; git -C /state/source add note.txt; "
        "git -C /state/source -c user.name=Fixture -c user.email=fixture@example.invalid commit --quiet -m 'Initial fixture'; fi")


def executor_resources(profile, image, ca):
    """The public executor example with this fixture's image and configuration,
    its published CA and the CLI's ServiceAccount. The executor mounts the
    sproozi-work-credentials Secret (tls.crt, tls.key and, for retained work,
    task-token), which the administrator creates from private files."""
    documents = list(yaml.safe_load_all((ROOT / "examples/portable-workflows/kubernetes/owner.yaml").read_text()))
    for doc in documents:
        if doc["kind"] == "Deployment":
            pod = doc["spec"]["template"]["spec"]
            pod["containers"][0]["image"] = image
            pod["containers"][0]["imagePullPolicy"] = "IfNotPresent"
            if any(r.get("snapshot") == "/state/source" for r in profile.get("repositories", {}).values()):
                # Only this credential-free fixture seeds an administrator-owned
                # Git snapshot; the public example uses approved URL sources.
                pod["initContainers"] = [{"name": "seed", "image": image, "imagePullPolicy": "IfNotPresent",
                    "command": ["/bin/sh", "-ec", SEED],
                    "securityContext": {"allowPrivilegeEscalation": False, "readOnlyRootFilesystem": True,
                                        "capabilities": {"drop": ["ALL"]}},
                    "resources": {"requests": {"cpu": "100m", "memory": "64Mi"}, "limits": {"cpu": "500m", "memory": "256Mi"}},
                    "volumeMounts": [{"name": "state", "mountPath": "/state"}, {"name": "tmp", "mountPath": "/tmp"}]}]
            # A changed configuration must restart the single executor.
            pod_meta = doc["spec"]["template"].setdefault("metadata", {})
            pod_meta.setdefault("annotations", {})["sproozi.test/profile"] = hashlib.sha256(
                json.dumps(profile, sort_keys=True).encode()).hexdigest()[:16]
    documents += [object_("ConfigMap", "sproozi-work-profile", data={"profile.json": json.dumps(profile, indent=2)}),
                  object_("ConfigMap", "sproozi-remote-executor", data={"ca.crt": ca})]
    documents.extend(doc for doc in build("config/cli-access") if doc)
    return documents


def replace_executor(documents, profile, image, ca):
    """Swap a manifest's executor resources for ones rendered from profile."""
    names = {(d["kind"], d["metadata"]["name"]) for d in executor_resources(profile, image, ca)}
    kept = [d for d in documents if d and (d["kind"], d["metadata"]["name"]) not in names]
    return kept + executor_resources(profile, image, ca)


def write_profile(path, profile):
    parent = path.parent
    stat = parent.lstat()
    if not parent.is_dir() or parent.is_symlink() or stat.st_uid != os.geteuid() or stat.st_mode & 0o077:
        raise ValueError("Profile output parent must be an existing private owned directory")
    if path.is_symlink():
        raise ValueError("Profile output cannot be a symlink")
    with tempfile.NamedTemporaryFile(mode="w", dir=parent, delete=False) as stream:
        temporary = Path(stream.name)
        try:
            json.dump(profile, stream, indent=2)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        except BaseException:
            temporary.unlink()
            raise
    os.replace(temporary, path)


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("control-image", "native-image"):
        parser.add_argument("--" + name, required=True)
    for name in ("hints", "ca-file", "gateway-cert", "gateway-key", "profile-config-output"):
        parser.add_argument("--" + name, type=Path, required=True)
    args = parser.parse_args()
    if not IMAGE.fullmatch(args.control_image) or not IMAGE.fullmatch(args.native_image):
        raise ValueError("Both images must use canonical repository@sha256 references")
    hints = hints_from(args.hints)
    ca = args.ca_file.read_text()
    if len(ca.encode()) > 1 << 20 or not re.fullmatch(r"(?:\s*-----BEGIN CERTIFICATE-----\s+[A-Za-z0-9+/=\s]+-----END CERTIFICATE-----\s*)+", ca):
        raise ValueError("CA file must contain only bounded PEM certificates")
    ssl.create_default_context(cadata=ca)
    trust = trust_name(ca)
    args.gateway_cert.resolve(strict=True), args.gateway_key.resolve(strict=True)
    documents = [doc for doc in build("config/default") if doc and doc["metadata"]["name"] in KEEP.get(doc["kind"], set())]
    documents.extend(doc for doc in build("config/agent-rbac") if doc)
    for doc in documents:
        if doc["kind"] == "Deployment":
            configure_deployment(doc, args.control_image, trust)
    documents.extend(fixture_resources(hints, args.native_image, ca, trust))
    profile = executor_profile(hints)
    documents.extend(executor_resources(profile, args.native_image, ca))
    # Validate and render everything before publishing either output.
    output = yaml.safe_dump_all(documents, sort_keys=False)
    write_profile(args.profile_config_output.absolute(), profile)
    sys.stdout.write(output)


if __name__ == "__main__":
    try:
        main()
    except (ValueError, KeyError, OSError, subprocess.SubprocessError, ssl.SSLError) as error:
        print("Mobility fixture render failed: " + str(error), file=sys.stderr)
        sys.exit(1)
