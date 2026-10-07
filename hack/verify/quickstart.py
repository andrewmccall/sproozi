#!/usr/bin/env python3
"""Drive the documented Kind paths in an owned checkout and retain their proof."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import threading
import time
from datetime import datetime, timezone
from uuid import uuid4


ROOT = Path(__file__).resolve().parents[2]
EVIDENCE_ROOT = ROOT / ".local/verification/quickstart"
REQUIRED_CRDS = {
    "agentruns.sproozi.com", "agentruntimes.sproozi.com",
    "agentpolicies.sproozi.com", "agenttemplates.sproozi.com",
}
CNI_POLICY_CRDS = {"adminnetworkpolicies.policy.networking.k8s.io",
                   "baselineadminnetworkpolicies.policy.networking.k8s.io"}
TOOLS = ["docker", "kind", "kubectl", "git", "go", "make", "openssl", "curl", "shasum"]
LOCK = EVIDENCE_ROOT / "driver.lock"


def capture(command, cwd=ROOT, env=None):
    return subprocess.run(command, cwd=cwd, env=env, capture_output=True, text=True,
                          timeout=30, check=True).stdout.strip()


def doctor(demo=False, env_file=None):
    """Read-only host checks; do not run setup or provider inference here."""
    missing = [tool for tool in TOOLS + (["gh"] if demo else []) if not shutil.which(tool)]
    if missing:
        raise RuntimeError("Missing tools: " + ", ".join(missing))
    versions = {
        "dockerServer": capture(["docker", "info", "--format", "{{.ServerVersion}}"]),
        "kind": capture(["kind", "version"]),
        "kubectl": json.loads(capture(["kubectl", "version", "--client", "-o", "json"])),
        "go": capture(["go", "version"]),
    }
    # The node image in the documented demo is Kubernetes 1.33.1.
    minor = int(versions["kubectl"]["clientVersion"]["minor"].rstrip("+"))
    if versions["kubectl"]["clientVersion"]["major"] != "1" or not 32 <= minor <= 34:
        raise RuntimeError("Use kubectl 1.32, 1.33 or 1.34 for the pinned Kubernetes 1.33.1 demo")
    if demo:
        if env_file is None or not env_file.is_file():
            raise RuntimeError("Demo needs --env-file pointing to a configured local environment")
        # Only report missing variable names. Values and credentials stay private.
        check = '''set -a; source "$1"; python3 - <<'PY'
import os, json
mode = os.environ.get('MODEL_AUTH_MODE', 'api_key')
required = ['GITHUB_APP_ID', 'GITHUB_APP_INSTALLATION_ID', 'GITHUB_APP_PRIVATE_KEY_FILE']
required += ['CHATGPT_SESSION_FILE'] if mode == 'chatgpt' else ['OPENAI_API_KEY']
print(json.dumps({'missing': [key for key in required if not os.environ.get(key)], 'mode': mode}))
PY'''
        auth = json.loads(capture(["bash", "-c", check, "quickstart-doctor", str(env_file)]))
        if auth["mode"] not in {"api_key", "chatgpt"} or auth["missing"]:
            raise RuntimeError("Configure model authentication and " + ", ".join(auth["missing"]))
        capture(["gh", "auth", "status"])
        versions["modelAuthMode"] = auth["mode"]
    return versions


def snapshot(destination):
    """Copy the current public sources, including unstaged edits and new guides."""
    names = capture(["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"])
    manifest = {}
    for name in sorted(set(names.split("\0")) - {""}):
        source = ROOT / name
        if not source.is_file():
            continue
        if source.is_symlink():
            raise RuntimeError("Verification source snapshots require regular files: " + name)
        target = destination / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copy2(source, target)
        manifest[name] = hashlib.sha256(source.read_bytes()).hexdigest()
    capture(["git", "init", "--quiet", str(destination)])
    return manifest


def run_command(command, work, env, log):
    with log.open("a") as output:
        output.write("\n$ " + " ".join(command) + "\n")
        output.flush()
        process = subprocess.Popen(command, cwd=work, env=env, stdout=output,
                                   stderr=subprocess.STDOUT, start_new_session=True)
        try:
            status = process.wait(timeout=3600)
        except BaseException:
            os.killpg(process.pid, signal.SIGTERM)
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
            raise
        if status:
            raise RuntimeError(f"Command exited {status}; see {log.name}")


def observe_crds(kubeconfig, stop, observed):
    """Read discovery while the existing gate installs and removes the CRDs."""
    while not stop.is_set():
        if kubeconfig.is_file():
            try:
                output = subprocess.run(
                    ["kubectl", "--kubeconfig", str(kubeconfig), "get", "crds", "-o", "json",
                     "--request-timeout=3s"], capture_output=True, text=True, timeout=5)
                if output.returncode == 0:
                    for crd in json.loads(output.stdout)["items"]:
                        observed[crd["metadata"]["name"]] = {
                            "group": crd["spec"]["group"], "kind": crd["spec"]["names"]["kind"],
                        }
            except (subprocess.TimeoutExpired, json.JSONDecodeError):
                pass  # The API is not ready yet, or the owned cluster is stopping.
        stop.wait(1)


def check_crds(observed):
    if not REQUIRED_CRDS.issubset(observed):
        raise RuntimeError("Did not observe all four Sproozi CRDs in the owned cluster")
    if not any(crd["group"] == "crd.projectcalico.org" for crd in observed.values()):
        raise RuntimeError("Did not observe the default Calico CRDs")
    unexpected = {name for name, crd in observed.items()
                  if name not in REQUIRED_CRDS | CNI_POLICY_CRDS
                  and crd["group"] != "crd.projectcalico.org"}
    if unexpected:
        raise RuntimeError("Unexpected external CRDs: " + ", ".join(sorted(unexpected)))


def owned_state(directory):
    directory = directory.resolve()
    if directory.parent != EVIDENCE_ROOT.resolve():
        raise RuntimeError("Cleanup requires one quickstart evidence directory")
    state = json.loads((directory / "owner.json").read_text())
    run_id = directory.name
    if not re.fullmatch(r"[0-9]{8}t[0-9]{6}z-[a-f0-9]{8}", run_id):
        raise RuntimeError("Invalid quickstart run identity")
    if state["path"] not in {"dependencies", "demo"}:
        raise RuntimeError("Unknown recorded verification path")
    expected = ("kind-sproozi-verify-" if state["path"] == "dependencies" else "sproozi-quickstart-") + run_id
    if state["cluster"] != expected:
        raise RuntimeError("Cluster does not match the recorded verification owner")
    return state


def cleanup(directory):
    state = owned_state(directory)
    cluster = state["cluster"]
    if cluster in capture(["kind", "get", "clusters"]).splitlines():
        subprocess.run(["kind", "delete", "cluster", "--name", cluster], check=True, timeout=120)
    if cluster in capture(["kind", "get", "clusters"]).splitlines():
        raise RuntimeError("Owned cluster remains after cleanup")
    work = directory / "checkout"
    if work.is_dir():
        shutil.rmtree(work)
    if LOCK.is_dir() and (LOCK / "owner").read_text().strip() == directory.name:
        (LOCK / "owner").unlink()
        LOCK.rmdir()
    result_file = directory / "result.json"
    if result_file.is_file():
        result = json.loads(result_file.read_text())
        result["clusterRemoved"] = True
        result["scratchRemoved"] = not work.exists()
        result_file.write_text(json.dumps(result, indent=2) + "\n")


def verify(args):
    versions = doctor(args.path == "demo", args.env_file)
    os.umask(0o077)
    run_id = datetime.now(timezone.utc).strftime("%Y%m%dt%H%M%Sz") + "-" + uuid4().hex[:8]
    directory = EVIDENCE_ROOT / run_id
    directory.mkdir(parents=True)
    work = directory / "checkout"
    cluster = ("kind-sproozi-verify-" if args.path == "dependencies" else "sproozi-quickstart-") + run_id
    if cluster in capture(["kind", "get", "clusters"]).splitlines():
        raise RuntimeError("Refusing to reuse an existing cluster")
    state = {"cluster": cluster, "path": args.path}
    (directory / "owner.json").write_text(json.dumps(state) + "\n")
    try:
        LOCK.mkdir()
    except FileExistsError:
        raise RuntimeError("Another quickstart owns driver.lock; finish or clean up its recorded run") from None
    (LOCK / "owner").write_text(run_id + "\n")
    result = {"path": args.path, "outcome": "failed", "toolVersions": versions,
              "cluster": cluster, "sourceCommit": capture(["git", "rev-parse", "HEAD"]),
              "guideSHA256": hashlib.sha256((ROOT / "docs/guides/getting-started.md").read_bytes()).hexdigest(),
              "features": {"dependencies": "not_run", "demo": "not_run", "exploration": "not_run"},
              "fullDemo": "not_run", "clusterRemoved": False, "scratchRemoved": False}
    stop, observed = threading.Event(), {}
    watcher = None
    print(f"Quickstart {args.path}: {directory}", flush=True)
    try:
        manifest = snapshot(work)
        (directory / "source-files.json").write_text(json.dumps(manifest, indent=2) + "\n")
        env = os.environ.copy()
        # Use only this run's context and evidence. Never inherit a developer cluster.
        env.update(SPROOZI_PRESERVE_ON_FAILURE="0", SPROOZI_VERIFY_ID=run_id,
                   CERT_MANAGER_INSTALL_SKIP="true", SPROOZI_E2E_METRICS="0",
                   SPROOZI_LIVE_REPORT=str(directory / "live.json"))
        local = work / ".local"
        local.mkdir()
        env_file = local / "demo.env"
        if args.path == "demo":
            shutil.copyfile(args.env_file, env_file)
        else:
            for key in ("SPROOZI_CNI_MANIFEST", "SPROOZI_CNI_SELECTOR"):
                env.pop(key, None)
        env["SPROOZI_DEMO_ENV_FILE"] = str(env_file)
        log = directory / "commands.log"
        if args.path == "dependencies":
            run_command(["bash", "hack/demo/setup.sh", "--network-only"], work, env, log)
            config = capture(["bash", "-c", 'source "$1"; printf "%s\\n%s" "$SPROOZI_CNI_MANIFEST" "$SPROOZI_CNI_SELECTOR"',
                              "cni-config", str(env_file)], cwd=work).splitlines()
            env.update(SPROOZI_CNI_MANIFEST=config[0], SPROOZI_CNI_SELECTOR=config[1])
            kubeconfig = directory / "cluster.kubeconfig"
            env.update(SPROOZI_KUBECONFIG=str(kubeconfig), KUBECONFIG=str(kubeconfig))
            command = ["make", "verify-kind"]
        else:
            kubeconfig = local / "demo" / (cluster + ".kubeconfig")
            command = ["make", "demo", "IMG=controller:sproozi-quickstart-" + run_id,
                       "KIND_CLUSTER=" + cluster]
        watcher = threading.Thread(target=observe_crds, args=(kubeconfig, stop, observed))
        watcher.start()
        run_command(command, work, env, log)
        stop.set()
        watcher.join()
        check_crds(observed)
        if args.path == "demo":
            live = json.loads((directory / "live.json").read_text())
            if live["outcome"] != "passed" or not all(live[key] for key in
                    ("observedSandbox", "denialProbesVerified", "cleanupVerified", "prURL", "prCommit")):
                raise RuntimeError("The live report did not prove the documented demo")
            result.update(fullDemo="passed", prURL=live["prURL"])
            result["features"]["demo"] = "passed"
            shutil.copyfile(kubeconfig, directory / "cluster.kubeconfig")
        result.update(outcome="passed", requiredCRDsObserved=True, externalSandboxCRDsRequired=False)
        result["features"]["dependencies"] = "passed"
    except (RuntimeError, OSError, subprocess.SubprocessError, KeyboardInterrupt) as error:
        result["error"] = str(error)
    finally:
        stop.set()
        if watcher is not None:
            watcher.join()
        (directory / "crds-observed.json").write_text(json.dumps(observed, indent=2) + "\n")
        (directory / "result.json").write_text(json.dumps(result, indent=2) + "\n")
        if args.keep_cluster and result["outcome"] == "passed":
            print("Owned cluster retained for exploration; run cleanup with this evidence directory", flush=True)
        else:
            try:
                cleanup(directory)
            except (RuntimeError, OSError, subprocess.SubprocessError) as error:
                result.update(outcome="failed", cleanupError=str(error))
                (directory / "result.json").write_text(json.dumps(result, indent=2) + "\n")
    print("Result: " + str(directory / "result.json"), flush=True)
    return 0 if result["outcome"] == "passed" else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="path", required=True)
    check = subparsers.add_parser("doctor", help="Read-only host and optional demo credential checks")
    check.add_argument("--env-file", type=lambda value: Path(value).resolve())
    check.add_argument("--evidence", type=lambda value: Path(value).resolve())
    subparsers.add_parser("dependencies", help="Prove the provider-free Kind dependency path")
    demo = subparsers.add_parser("demo", help="Run the documented real AgentRun-to-PR demo")
    demo.add_argument("--env-file", required=True, type=lambda value: Path(value).resolve())
    demo.add_argument("--keep-cluster", action="store_true", help="Retain a successful owned cluster for exploration")
    remove = subparsers.add_parser("cleanup", help="Remove only the recorded cluster and scratch checkout")
    remove.add_argument("evidence", type=lambda value: Path(value).resolve())
    args = parser.parse_args()
    if args.path == "doctor":
        health = doctor(args.env_file is not None, args.env_file)
        if args.evidence:
            state = owned_state(args.evidence)
            config = args.evidence / "cluster.kubeconfig"
            context = capture(["kubectl", "--kubeconfig", str(config), "config", "current-context"])
            if context != "kind-" + state["cluster"]:
                raise RuntimeError("Kubeconfig context does not match the owned cluster")
            capture(["kubectl", "--kubeconfig", str(config), "get", "nodes", "--request-timeout=5s"])
            health.update(cluster=state["cluster"], context=context, apiReachable=True)
        print(json.dumps(health, indent=2))
        return 0
    if args.path == "cleanup":
        cleanup(args.evidence)
        return 0
    if args.path == "dependencies":
        args.env_file, args.keep_cluster = None, False
    return verify(args)


def interrupted(*_):
    raise KeyboardInterrupt()


if __name__ == "__main__":
    signal.signal(signal.SIGTERM, interrupted)
    try:
        raise SystemExit(main())
    except (RuntimeError, OSError, subprocess.SubprocessError) as error:
        raise SystemExit(str(error)) from error
