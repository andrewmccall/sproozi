"""Optional administrator denials using the extended proof's fixed native script.

This is fixture code, not a worker control API. It runs the fixed existing probe
inside exact admitted containers and records only safe observations/identities.
No Kubernetes configuration or effective native environment enters report JSON.
"""
import json
import hashlib
from pathlib import Path
import re


def require(condition, message):
    if not condition:
        raise RuntimeError(message)


class NativeDenials:
    def __init__(self, proof):
        self.proof = proof
        args = proof.args
        require(args.kubeconfig is not None, "Administrator probes require a dedicated Kind kubeconfig")
        for namespace in (args.namespace, args.agent_namespace):
            require(re.fullmatch(r"[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?", namespace), "Invalid proof namespace")
        self.context = proof.run([str(args.kubectl), "--kubeconfig", str(args.kubeconfig.resolve(strict=True)),
                                  "config", "current-context"], "probe-context", timeout=15).decode().strip()
        require(re.fullmatch(r"kind-sproozi-mobility-[a-z0-9-]+", self.context),
                "Administrator probes require a dedicated mobility Kind context")

    def docker(self, *arguments, data=None):
        c = self.proof.config
        return self.proof.run([c["dockerBinary"], "--host", "unix://" + c["dockerSocket"],
                              "--config", c["dockerConfig"], *arguments], "docker-probe", data=data, timeout=35)

    def kube(self, namespace, *arguments, data=None):
        a = self.proof.args
        return self.proof.run([str(a.kubectl), "--kubeconfig", str(a.kubeconfig.resolve()), "--context", self.context,
                              "--namespace", namespace, *arguments], "kube-probe", data=data, timeout=35)

    def local(self, active):
        p = self.proof
        records = []
        for path in (Path(p.config["directory"]) / "docker").glob("*.json"):
            require(path.stat().st_size <= 1 << 20, "Docker placement record exceeds fixture bound")
            record = json.loads(path.read_text())
            launch = record.get("launch", {})
            if launch.get("sessionId") == p.id and (active is None or launch == active["launch"]):
                records.append(record)
        require(len(records) == 1, "Denial probe requires one exact local allocation")
        r = records[0]
        agent, launch = r["Agent"], r["launch"]
        require(launch["harness"] == p.config["local"]["Harness"] and launch["placement"] == p.config["local"]["Name"] and
                agent["Execution"] == launch["allocationId"], "Local probe allocation binding changed")
        if active:
            require(agent["ID"] == active["incarnation"]["resourceUid"], "Local probe incarnation changed")
        def confirm():
            observed = json.loads(self.docker("container", "inspect", agent["ID"]))
            require(len(observed) == 1 and observed[0]["Id"] == agent["ID"] and observed[0]["State"]["Running"] and
                    observed[0]["Config"]["Labels"]["sproozi.com/execution"] == launch["allocationId"],
                    "Exact local native container is unavailable or replaced")
        confirm()
        return ["docker", agent["ID"]], {"allocationId": launch["allocationId"], "containerId": agent["ID"]}, confirm

    def remote(self, active):
        a, p = self.proof.args, self.proof
        launch, instance = active["launch"], active["incarnation"]
        configs = json.loads(self.kube(a.namespace, "get", "configmaps", "-l",
                                      "sessions.sproozi.com/registry=true", "-o", "json"))["items"]
        records = [json.loads(c["data"]["registration.json"]) for c in configs if c.get("data", {}).get("registration.json")]
        matches = [r for r in records if r["registration"]["sessionId"] == p.id and
                   r["runUid"] == instance["executionId"]]
        require(len(matches) == 1, "Remote probe requires one exact allocation registration")
        r = matches[0]
        require(r["phase"] == "active" and r["registration"]["epoch"] == launch["epoch"] and
                r["registration"]["operationId"] == launch["operationId"] and
                r["registration"]["harness"] == p.config["local"]["Harness"] and
                r["registration"]["nativeUuid"] == launch["threadId"] and
                r["registration"]["intendedRunName"] == "session-" + hashlib.sha256(launch["allocationId"].encode()).hexdigest()[:40] and
                r["runUid"] == instance["executionId"] and r["podUid"] == instance["resourceUid"],
                "Remote probe registration changed exact admitted incarnation")
        pods = json.loads(self.kube(a.agent_namespace, "get", "pods", "-o", "json"))["items"]
        matches = [pod for pod in pods if pod["metadata"]["uid"] == instance["resourceUid"]]
        require(len(matches) == 1, "Exact remote source Pod is unavailable")
        name = matches[0]["metadata"]["name"]
        def confirm():
            pod = json.loads(self.kube(a.agent_namespace, "get", "pod", name, "-o", "json"))
            require(pod["metadata"]["uid"] == instance["resourceUid"] and not pod["metadata"].get("deletionTimestamp") and
                    any(c["name"] == "agent" and "running" in c["state"] for c in pod["status"].get("containerStatuses", [])),
                    "Exact remote native Pod is unavailable or replaced")
        confirm()
        status = json.loads(self.kube(a.agent_namespace, "exec", name, "-c", "agent", "--", "/usr/local/bin/native-session", "status"))
        require(not status.get("error") and status.get("state") == "active" and status.get("threadId") == launch["threadId"],
                "Remote native source lost its actual UUID")
        return ["kube", name], {"allocationId": launch["allocationId"], "runUid": r["runUid"], "podUid": r["podUid"]}, confirm

    def run(self, active=None):
        p = self.proof
        local = active is None or active["launch"]["placement"] == p.config["local"]["Name"]
        target, binding, confirm = self.local(active) if local else self.remote(active)
        source = Path(__file__).with_name("native-denials.js").read_bytes()
        require(len(source) <= 64 << 10, "Fixed native denial probe exceeds fixture bound")
        if local:
            data = self.docker("exec", "--interactive", "--user", "1000", target[1], "/usr/local/bin/node", data=source)
        else:
            data = self.kube(p.args.agent_namespace, "exec", "--stdin", target[1], "-c", "agent", "--",
                             "/usr/local/bin/node", data=source)
        observed = json.loads(data)
        require(observed["passed"] and observed["nativeUID"] == 1000 and observed["firewallMutationDenied"],
                "Actual native worker denials failed")
        confirm()
        # Use only the fixed script's non-secret result fields in public evidence.
        observed = {k: observed[k] for k in ("passed", "nativeUID", "capabilityDenials", "directDenials", "firewallMutationDenied")}
        p.report["steps"].append({"nativeDenialsPlacement": p.config["local"]["Name"] if local else active["launch"]["placement"],
                                  "binding": binding, "observed": observed})
        p.save()
