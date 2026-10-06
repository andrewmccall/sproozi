#!/usr/bin/env python3
"""Prove stock Codex MCP use and revocation in a caller-provisioned Kind cluster."""

import argparse
import json
from pathlib import Path
import re
import shlex
import subprocess
import time
import uuid
from datetime import datetime, timezone


RUN_LABEL = "sproozi.com/agentrun-uid"
AGENTS = "sproozi-agents"
SYSTEM = "sproozi-system"
ENDPOINT = "https://kubernetes.default.svc/mcp"
TOKEN = "/var/run/sproozi/tokens/gateway/token"
CA = "/etc/sproozi/trust/ca-bundle.pem"


def timestamp():
    return datetime.now(timezone.utc).isoformat()


def require_no_sandbox_secrets(spec):
    """Check every Pod credential source, including auxiliary containers."""
    for field in ("containers", "initContainers", "ephemeralContainers"):
        for container in spec.get(field, []):
            if any("secretKeyRef" in env.get("valueFrom", {}) for env in container.get("env", [])):
                raise RuntimeError("Sandbox must not reference Secrets")
            if any("secretRef" in source for source in container.get("envFrom", [])):
                raise RuntimeError("Sandbox must not reference Secrets")
    if spec.get("imagePullSecrets") or any(
        "secret" in volume or any("secret" in source for source in volume.get("projected", {}).get("sources", []))
        for volume in spec.get("volumes", [])
    ):
        raise RuntimeError("Sandbox must not reference Secrets")


def wait_for(check, description, timeout=180):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = check()
        if value:
            return value
        time.sleep(2)
    raise RuntimeError(f"Timed out waiting for {description}")


class Cluster:
    def __init__(self, kubeconfig):
        self.prefix = ["kubectl", "--kubeconfig", kubeconfig, "--request-timeout=15s"]

    def command(self, *args, document=None, allow_failure=False, timeout=45):
        result = subprocess.run(
            self.prefix + list(args), input=document, text=True,
            capture_output=True, timeout=timeout,
        )
        if result.returncode and not allow_failure:
            # Avoid embedding workload output or credential material in errors.
            raise RuntimeError(f"kubectl {args[0]} failed with exit {result.returncode}")
        return result

    def get(self, resource, name=None, namespace=SYSTEM):
        args = ["get", resource]
        if name:
            args.append(name)
        return json.loads(self.command(*args, "-n", namespace, "-o", "json").stdout)

    def create(self, obj):
        self.command("create", "-f", "-", document=json.dumps(obj))

    def exec(self, pod, script):
        return self.command(
            "exec", pod, "-n", AGENTS, "-c", "agent", "--", "/bin/sh", "-ec", script,
            allow_failure=True,
        )

    def mcp(self, pod, tool, namespace=None, *, server=None, arguments=None):
        body = json.dumps({"jsonrpc": "2.0", "id": 1, "method": "tools/call",
                           "params": {"name": tool, "arguments": arguments if arguments is not None else {"namespace": namespace}}})
        endpoint = ENDPOINT if server is None else "https://sproozi-gateway.sproozi-system.svc/mcp/" + server
        # All arguments are constants or validated DNS names. The run token is
        # read only inside the workload and never returned to the verifier.
        script = (
            f'token="$(cat {TOKEN})"\n'
            'proxy="https://sproozi:${token}@${SPROOZI_GATEWAY#https://}"\n'
            f'curl --silent --max-time 10 --proxy "$proxy" --proxy-cacert {CA} '
            f'--cacert {CA} -H "Content-Type: application/json" '
            '-H "Accept: application/json, text/event-stream" '
            f"--data {shlex.quote(body)} --write-out '\nSPROOZI_STATUS:%{{http_connect}}:%{{http_code}}\n' {shlex.quote(endpoint)}"
        )
        result = self.exec(pod, script)
        body, separator, status = result.stdout.rpartition("\nSPROOZI_STATUS:")
        if not separator:
            raise RuntimeError("MCP probe did not return HTTP status evidence")
        connect, response = status.strip().split(":")
        try:
            payload = json.loads(body)
        except ValueError:
            payload = None
        return connect, response, payload


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--kubeconfig", required=True)
    parser.add_argument("--cluster", required=True, help="Caller-owned dedicated Kind cluster")
    parser.add_argument("--mode", choices=("native", "configured"), default="native")
    parser.add_argument("--template", default="mcp-proof")
    parser.add_argument("--report", required=True)
    parser.add_argument("--timeout", type=int, default=600)
    args = parser.parse_args(argv)
    for value in [args.cluster, args.template]:
        if not re.fullmatch(r"[a-z0-9]([-a-z0-9]*[a-z0-9])?", value):
            parser.error("Cluster, template and run names must be DNS labels")
    if not Path(args.kubeconfig).is_file() or args.timeout < 30:
        parser.error("Supply an existing kubeconfig and timeout of at least 30 seconds")
    k = Cluster(str(Path(args.kubeconfig).resolve()))
    run_name = "mcp-live-" + uuid.uuid4().hex[:16]
    report = {"outcome": "failed", "startedAt": timestamp(), "cluster": args.cluster,
              "runName": run_name, "template": args.template, "checks": {}, "limitations": [
                  "Bounded MCP read/model proof; no GitHub publication or deployed remediation",
                  "Normal provisioned-run cancellation; interrupted provisioning recovery is not covered",
                  "Cleanup covers run-labelled invocation resources; durable budget state is retained",
                  "Observed model usage does not establish a hard provider-spending ceiling",
                  "Witness Pod is verifier-owned and independently network-isolated",
              ]}
    capabilities = ["model.inference", "kubernetes.read"] if args.mode == "native" else ["model.inference", "mcp.docs", "mcp.inventory"]
    report["mode"] = args.mode
    witness = None
    run_uid = None
    created = False
    try:
        context = k.command("config", "current-context").stdout.strip()
        clusters = subprocess.check_output(["kind", "get", "clusters"], text=True).splitlines()
        if context != "kind-" + args.cluster or args.cluster not in clusters:
            raise RuntimeError("The explicit kubeconfig must select the named dedicated Kind cluster")
        nodes = k.get("nodes", namespace=SYSTEM)["items"]
        if len(nodes) < 2 or any(not n["metadata"]["name"].startswith(args.cluster + "-") for n in nodes):
            raise RuntimeError("Use a dedicated multi-node Kind cluster with worker placement")
        k.command("wait", "pods", "-n", "kube-system", "-l", "k8s-app=calico-node", "--for=condition=Ready", "--timeout=60s", timeout=75)
        # The random invocation name is exclusively owned by this verifier.
        # A timed-out create may have succeeded, so cleanup covers the attempt.
        created = True
        k.create({"apiVersion": "sproozi.com/v1alpha1", "kind": "AgentRun",
                  "metadata": {"name": run_name, "namespace": SYSTEM},
                  "spec": {"templateRef": {"name": args.template},
                           "task": "Use the approved MCP tools, report the observed names and finish.",
                           "eventContext": {"trigger": "mcp-live-proof"},
                           "capabilities": capabilities}})
        run = k.get("agentrun", run_name)
        if run["spec"]["templateRef"]["name"] != args.template or set(run["spec"]["capabilities"]) != set(capabilities):
            raise RuntimeError("Run must use the selected proof template and only the proof grants")
        run_uid = run["metadata"]["uid"]
        report["runUID"] = run_uid
        def running_pod():
            current = k.get("agentrun", run_name)
            if current.get("status", {}).get("phase") in {"Failed", "Succeeded", "Cancelled", "TimedOut"}:
                raise RuntimeError("Run ended before the live probes could execute")
            name = current.get("status", {}).get("identity", {}).get("sandboxName")
            if not name:
                return None
            pod = k.get("pod", name, AGENTS)
            return pod if pod["status"]["phase"] == "Running" else None
        pod = wait_for(running_pod, "stock Codex sandbox", args.timeout)
        pod_name = pod["metadata"]["name"]
        if not pod["spec"]["nodeName"].endswith("-worker"):
            raise RuntimeError("Sandbox must run on the worker, away from API-server node traffic exceptions")
        container = next(c for c in pod["spec"]["containers"] if c["name"] == "agent")
        if "@sha256:" not in container["image"]:
            raise RuntimeError("Require a pinned stock image")
        require_no_sandbox_secrets(pod["spec"])
        report["checks"]["noSandboxSecretReferences"] = True
        report["codexImage"] = container["image"]
        report["codexVersion"] = k.exec(pod_name, "codex --version").stdout.strip()
        report["placement"] = pod["spec"]["nodeName"]
        def codex_finished():
            logs = k.command("logs", pod_name, "-n", AGENTS, "-c", "agent").stdout
            if "SPROOZI_MCP_AGENT_FINISHED" not in logs.splitlines():
                return None
            if "SPROOZI_MCP_EXIT:0" not in logs.splitlines():
                raise RuntimeError("Stock Codex exited unsuccessfully")
            tools = []
            for line in logs.splitlines():
                try:
                    item = json.loads(line).get("item", {})
                except ValueError:
                    continue
                if item.get("type") == "mcp_tool_call":
                    tools.append(item)
            if not tools:
                raise RuntimeError("Stock Codex finished without an MCP tool call")
            return tools
        calls = wait_for(codex_finished, "completed Codex MCP call", args.timeout)
        if args.mode == "native":
            allowed = [c for c in calls if c.get("server") == "sproozi-kubernetes" and c.get("tool") == "kubernetes_list_pods"
                       and c.get("status") == "completed" and c.get("arguments") == {"namespace": "sproozi-demo"}]
            if not allowed:
                raise RuntimeError("Stock Codex did not complete the requested MCP call")
            tool_result = allowed[0].get("result") or {}
            if tool_result.get("isError") or allowed[0].get("error"):
                raise RuntimeError("Stock Codex MCP tool returned an error")
            content = tool_result.get("content", [])
            lists = [json.loads(c["text"]) for c in content if c.get("type") == "text"]
            names = [p["metadata"]["name"] for doc in lists if doc.get("kind") == "PodList" for p in doc.get("items", [])]
            if not any(name.startswith("mcp-observed-workload-") for name in names):
                raise RuntimeError("Codex result did not contain the real observed workload Pod")
            report["mcpCall"] = {"server": "sproozi-kubernetes", "tool": "kubernetes_list_pods",
                                 "namespace": "sproozi-demo", "status": "completed", "observedPodNames": names}
            report["checks"]["stockCodexMCP"] = True
            print("Stock Codex completed the MCP tool against real Kubernetes", flush=True)
            for tool, namespace, label in [("kubernetes_list_pods", "kube-system", "scopeDenial"),
                                            ("kubernetes_delete_pods", "sproozi-demo", "unknownToolDenial")]:
                connect, response, payload = k.mcp(pod_name, tool, namespace)
                if connect != "200" or response != "200" or not payload or not (payload.get("error") or payload.get("result", {}).get("isError")):
                    raise RuntimeError(f"MCP {label} was not enforced")
                report["checks"][label] = True
            print("MCP scope and unknown-tool denials passed", flush=True)
        else:
            expected_calls = [
                ("docs", "search", {"collection": "operations", "query": "runbook"},
                 {"document": "operations-runbook", "collection": "operations"}),
                ("inventory", "list_assets", {"tenant": "home-ops", "kind": "router"},
                 {"assets": ["home-router"], "tenant": "home-ops"}),
            ]
            evidence_calls = []
            for server, tool, arguments, expected in expected_calls:
                approved = [call for call in calls if call.get("server") == server and call.get("tool") == tool
                            and call.get("status") == "completed" and call.get("arguments") == arguments]
                if not approved or approved[0].get("error") or (approved[0].get("result") or {}).get("isError"):
                    raise RuntimeError("Stock Codex did not complete both configured MCP calls")
                texts = [json.loads(content["text"]) for content in approved[0]["result"].get("content", []) if content.get("type") == "text"]
                if expected not in texts:
                    raise RuntimeError("Configured tool result differs from the deployed fixture")
                evidence_calls.append({"server": server, "tool": tool, "status": "completed", "result": expected})
            report["mcpCalls"] = evidence_calls
            report["checks"]["stockCodexMCP"] = True
            print("Stock Codex completed both configured MCP providers", flush=True)
            denied = [
                ("docs", "search", {"collection": "private", "query": "runbook"}, "docsScopeDenial"),
                ("inventory", "list_assets", {"tenant": "other", "kind": "router"}, "inventoryScopeDenial"),
                ("docs", "search", {"collection": "operations"}, "providerSchemaDenial"),
                ("docs", "delete_all", {}, "unknownToolDenial"),
                ("unselected", "search", {"collection": "operations", "query": "runbook"}, "unselectedServerDenial"),
            ]
            for server, tool, arguments, label in denied:
                connect, response, payload = k.mcp(pod_name, tool, server=server, arguments=arguments)
                if connect != "200" or not (response in {"400", "403"} or
                    response == "200" and payload and (payload.get("error") or payload.get("result", {}).get("isError"))):
                    raise RuntimeError(f"MCP {label} was not enforced")
                report["checks"][label] = True
        service = k.get("service", "kubernetes", "default")
        control = next(n for n in nodes if n["metadata"]["name"].endswith("-control-plane"))
        control_ip = next(a["address"] for a in control["status"]["addresses"] if a["type"] == "InternalIP")
        targets = [service["spec"]["clusterIP"] + ":443", control_ip + ":6443"]
        if args.mode == "configured":
            targets += [k.get("service", "mcp-" + name, SYSTEM)["spec"]["clusterIP"] + ":8443" for name in ("docs", "inventory")]
        for target in targets:
            probe = k.exec(pod_name, "set +e\n" + f"curl --silent --noproxy '*' --connect-timeout 3 --max-time 5 -k -o /dev/null https://{target}/api\n" + "code=$?\ntest \"$code\" = 28")
            if probe.returncode:
                raise RuntimeError("Direct upstream access did not time out at the network boundary")
        report["checks"]["directAPIDenial"] = True
        if args.mode == "configured":
            report["checks"]["directProviderDenial"] = True
        print("Direct upstream access timed out", flush=True)
        # A verifier-owned witness keeps its own containment after the original
        # run Pod is stopped, allowing an actual stale-identity network probe.
        witness = "mcp-witness-" + run_uid[:8]
        spec = pod["spec"]
        spec["containers"] = [dict(container, command=["/bin/sh", "-ec"], args=["sleep 900"])]
        labels = {"sproozi.com/agentrun": run_name, "sproozi.com/mcp-witness": witness}
        policies = k.get("networkpolicies", namespace=AGENTS)["items"]
        network = next(p for p in policies if p["metadata"].get("labels", {}).get(RUN_LABEL) == run_uid)
        network["metadata"] = {"name": witness, "namespace": AGENTS, "labels": {"sproozi.com/mcp-witness": witness}}
        network["spec"]["podSelector"] = {"matchLabels": {"sproozi.com/mcp-witness": witness}}
        k.create(network)
        k.create({"apiVersion": "v1", "kind": "Pod", "metadata": {"name": witness, "namespace": AGENTS, "labels": labels}, "spec": spec})
        k.command("wait", "pod", witness, "-n", AGENTS, "--for=condition=Ready", "--timeout=120s", timeout=135)
        probe_args = {} if args.mode == "native" else {"server": "docs", "arguments": {"collection": "operations", "query": "runbook"}}
        probe_tool = "kubernetes_list_pods" if args.mode == "native" else "search"
        connect, response, payload = k.mcp(witness, probe_tool, "sproozi-demo", **probe_args)
        if connect != "200" or response != "200" or not payload or payload.get("error") or not payload.get("result", {}).get("content") or payload["result"].get("isError"):
            raise RuntimeError("Witness did not share the active run's approved authority")
        if args.mode == "configured":
            connect, response, payload = k.mcp(witness, probe_tool, "sproozi-demo", **probe_args)
            if connect != "200" or response != "200" or not payload or payload.get("error") or payload.get("result", {}).get("isError"):
                raise RuntimeError("Remaining configured MCP request budget was unavailable")
            connect, response, payload = k.mcp(witness, probe_tool, "sproozi-demo", **probe_args)
            if connect != "200" or response != "200" or not payload or not (payload.get("error") or payload.get("result", {}).get("isError")):
                raise RuntimeError("Configured MCP request budget was not enforced across connections")
            report["checks"]["reconnectBudgetDenial"] = True
        k.command("patch", "agentrun", run_name, "-n", SYSTEM, "--type=merge", "-p", '{"spec":{"cancel":true}}')
        cancelled = wait_for(lambda: k.get("agentrun", run_name).get("status", {}).get("phase") == "Cancelled", "Cancelled phase", 120)
        report["checks"]["cancelled"] = bool(cancelled)
        connect, response, _ = k.mcp(witness, probe_tool, "sproozi-demo", **probe_args)
        if connect not in {"401", "403"} and response not in {"401", "403"}:
            raise RuntimeError("Run identity retained MCP authority after cancellation")
        report["checks"]["revokedIdentityDenial"] = True
        print("Cancelled run identity was denied through the isolated witness", flush=True)
        def resources_gone():
            for namespace in [AGENTS, SYSTEM]:
                remaining = k.command("get", "pods,serviceaccounts,configmaps,networkpolicies,roles,rolebindings", "-n", namespace,
                                      "-l", RUN_LABEL + "=" + run_uid, "-o", "json").stdout
                if json.loads(remaining)["items"]:
                    return False
            return True
        wait_for(resources_gone, "controller-owned resources to be released", 180)
        report["checks"]["controllerCleanup"] = True
        print("Run-labelled controller resources were released", flush=True)
        audit = k.command("logs", "deployment/sproozi-gateway", "-n", SYSTEM).stdout
        evidence = []
        for line in audit.splitlines():
            try:
                event = json.loads(line)
            except ValueError:
                continue
            if event.get("runUID") == run_uid:
                evidence.append({key: event.get(key) for key in ["operation", "capability", "semanticLevel", "allowed", "upstreamStatus", "tokensUsed"]})
        if args.mode == "native" and not any(e["operation"] == "kubernetes.list" and e["semanticLevel"] == "semantic" and e["allowed"] for e in evidence):
            raise RuntimeError("Missing correlated semantic Kubernetes audit evidence")
        report["auditEvidence"] = evidence
        if args.mode == "native":
            report["checks"]["semanticAudit"] = True
        else:
            for capability in ("mcp.docs", "mcp.inventory"):
                if not any(e["operation"] == "mcp.tools.call" and e["semanticLevel"] == "protocol" and e["capability"] == capability and e["allowed"] for e in evidence):
                    raise RuntimeError("Missing correlated configured MCP audit evidence")
            report["checks"]["protocolAudit"] = True
            for name, tool, expected_count in [("docs", "search", 3), ("inventory", "list_assets", 1)]:
                logs = k.command("logs", "deployment/mcp-" + name, "-n", SYSTEM).stdout
                if "UNAPPROVED_TOOL_CALL" in logs or logs.count("APPROVED_TOOL_CALL " + name + " " + tool) != expected_count:
                    raise RuntimeError("Provider invocation counts do not match approved calls")
            report["providerCalls"] = {"docs": 3, "inventory": 1}
            report["checks"]["denialsPreventProviderCalls"] = True
        model_calls = [e for e in evidence if e["capability"] == "model.inference" and e["allowed"] and e["upstreamStatus"] == 200 and isinstance(e["tokensUsed"], int)]
        if not model_calls:
            raise RuntimeError("Missing correlated successful provider usage")
        report["successfulModelRequests"] = len(model_calls)
        report["observedTotalModelTokens"] = sum(e["tokensUsed"] for e in model_calls)
        report["outcome"] = "passed"
    except Exception as error:
        report["error"] = str(error) if isinstance(error, RuntimeError) else type(error).__name__
        raise
    finally:
        # Delete only this gate's proof run and witness, even if reading the
        # newly created Run failed. Preserve a report when cleanup also fails.
        cleanup_failed = False
        cleanup = []
        if created:
            cleanup.append(("delete", "agentrun", run_name, "-n", SYSTEM, "--ignore-not-found", "--wait=true", "--timeout=90s"))
        if witness:
            cleanup.append(("delete", "pod,networkpolicy", witness, "-n", AGENTS, "--ignore-not-found", "--wait=true", "--timeout=90s"))
        for command in cleanup:
            try:
                result = k.command(*command, allow_failure=True, timeout=105)
                cleanup_failed |= result.returncode != 0
            except (subprocess.TimeoutExpired, OSError):
                cleanup_failed = True
        if cleanup_failed:
            report["outcome"] = "failed"
            report["cleanupError"] = "Verifier cleanup failed; inspect the named proof resources"
        report["finishedAt"] = timestamp()
        path = Path(args.report)
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(json.dumps(report, indent=2) + "\n")
        print(f"Redacted MCP acceptance report: {path}", flush=True)
        if cleanup_failed:
            raise RuntimeError(report["cleanupError"])
    print("MCP Kind acceptance passed", flush=True)


if __name__ == "__main__":
    main()
