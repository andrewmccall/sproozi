#!/usr/bin/env python3
"""Prove remote native task retention and fresh home continuation in owned fixtures."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import ssl
import subprocess
import time
import urllib.error
import urllib.request
import uuid

import hermes
import governance

ROOT = Path(__file__).resolve().parents[3]
spec = importlib.util.spec_from_file_location("mobility_host", ROOT / "hack/verify/session-mobility/host.py")
HOST = importlib.util.module_from_spec(spec)
spec.loader.exec_module(HOST)


def check(value, message):
    if not value:
        raise RuntimeError(message)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("home-config", "profile-config", "session-binary", "pty-helper", "output-directory"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--request-id", help="Retained original submission UUID for recovery proof")
    parser.add_argument("--hermes-deployment", help="Deployed stock Hermes in sproozi-coordinators; enables native caller proof")
    parser.add_argument("--hermes-kubeconfig", type=Path, help="Explicit isolated Kind kubeconfig for the native Hermes caller")
    parser.add_argument("--home-ops", action="store_true", help="Exercise approved Pod read and prohibited Secrets read in each placement")
    parser.add_argument("--project", help="Configured project ID; prove MCP and CLI filtering and local association")
    parser.add_argument("--build-sproozi", action="store_true", help="Run actual offline package tests remotely and locally")
    parser.add_argument("--replay-revision", action="store_true", help="Start independently bounded fresh native work at the original recorded Git HEAD")
    args = parser.parse_args()
    check(not args.build_sproozi or not args.home_ops, "Choose one dogfooding workflow per proof")
    check(bool(args.hermes_deployment) == bool(args.hermes_kubeconfig),
          "Use --hermes-deployment and --hermes-kubeconfig together")
    check(not args.home_ops or args.hermes_kubeconfig, "Home-ops audit proof needs the explicit isolated kubeconfig")
    os.umask(0o077)
    out = HOST.private_directory(args.output_directory)
    check(not (out / "report.json").exists(), "Use a fresh proof output directory")
    home = json.loads(args.home_config.read_text())
    profile = json.loads(args.profile_config.read_text())
    check(Path(home["gateway"]["providerTokenFile"]).read_text().strip() == HOST.TOKEN,
          "This verifier accepts only the deterministic public provider fixture")
    source = profile["placementName"]
    origin = home["remote"][source]["url"]
    tls = ssl.create_default_context(cafile=home["remote"][source]["caFile"])
    task_token = Path(profile["workTokenFile"]).read_text().strip()
    socket = Path(home["directory"]) / "control.sock"
    check(not socket.exists(), "First remote task must run without the home daemon")
    report = {"passed": False, "scope": "genuine stock native task via authenticated MCP; remote retention and fresh local resume",
              "limitations": ["Deterministic provider; no paid inference or real engineering contribution claim",
                              "MCP caller emulates Hermes's supported native client, not a fresh Hermes harness proof",
                              "Administrator-seeded workspace; personal infrastructure is never accessed"], "steps": []}
    if args.hermes_deployment:
        report["limitations"].remove("MCP caller emulates Hermes's supported native client, not a fresh Hermes harness proof")
        report["hermesDeployment"] = args.hermes_deployment
    def save():
        HOST.write(out / "report.json", report)
    def post(path, body, token=task_token):
        request = urllib.request.Request(origin + path, data=json.dumps(body).encode(), method="POST",
                                         headers={"Authorization": "Bearer " + token, "Content-Type": "application/json",
                                                  "Accept": "application/json, text/event-stream"})
        try:
            with urllib.request.urlopen(request, context=tls, timeout=45) as response:
                return json.loads(response.read())
        except urllib.error.HTTPError as error:
            error.read()
            error.close()
            raise
    def tool(name, arguments, call_id):
        reply = post("/work/mcp", {"jsonrpc": "2.0", "id": call_id, "method": "tools/call",
                                   "params": {"name": name, "arguments": arguments}})
        check("error" not in reply and not reply["result"].get("isError"), "MCP tool failed")
        return reply["result"]["structuredContent"]
    save()
    ready = time.monotonic() + 15
    while True:
        try:
            initialized = post("/work/mcp", {"jsonrpc": "2.0", "id": 1, "method": "initialize",
                                            "params": {"protocolVersion": "2025-03-26", "capabilities": {},
                                                       "clientInfo": {"name": "portable-workflow-proof", "version": "1"}}})
            break
        except urllib.error.HTTPError:
            raise
        except urllib.error.URLError:
            if time.monotonic() >= ready:
                raise
            time.sleep(0.1)
    check("result" in initialized, "MCP initialization failed")
    intent = {"requestId": args.request_id or str(uuid.uuid4()), "harness": "codex", "repository": "sproozi",
              "executor": source, "task": HOST.HOMEOPS_PROMPT if args.home_ops else HOST.BUILD_PROMPT if args.build_sproozi else HOST.PROMPTS[0]}
    if args.project:
        intent["project"] = args.project
    report["requestId"] = intent["requestId"]
    report['localNativeImageID'] = home['local']['NativeImageID']
    seed = args.profile_config.parent / 'source-seed.json'
    if args.build_sproozi:
        check(seed.exists(), "Seed public Sproozi compiler sources before development proof")
        report['sourceSeed'] = json.loads(seed.read_text())
    if args.hermes_kubeconfig:
        runtime = subprocess.run(['kubectl', '--kubeconfig', str(args.hermes_kubeconfig.resolve()), '-n', profile['namespace'],
                                  'get', 'agentruntime', profile['templateName'], '-o', 'json'],
                                 capture_output=True, text=True, check=True)
        containers = json.loads(runtime.stdout)['spec']['podTemplate']['spec']['containers']
        report['remoteNativeImage'] = next(c['image'] for c in containers if c['name'] == 'agent')
    save()
    def native_hermes(marker, request, name):
        return hermes.invoke(directory=out / name, kubeconfig=args.hermes_kubeconfig,
                             deployment=args.hermes_deployment, marker=marker, intent=request)
    if args.hermes_deployment:
        view = native_hermes("SPROOZI_WORK_REQUEST=", intent, "hermes-dispatch")
        report["steps"].append("Stock Hermes discovered and invoked the retained-work MCP tool")
    else:
        view = tool("sproozi_run", intent, 2)
    report["sessionId"] = view["id"]
    report["executionId"] = view["executionId"]
    report["allocationId"] = view["allocationId"]
    save()
    if args.request_id and view["phase"] == "unknown":
        recovered = subprocess.run([str(args.session_binary.resolve()), "recover", view["id"],
                                    "--executor", source, "--config", str(args.home_config.resolve())],
                                   cwd=ROOT, capture_output=True, text=True, timeout=120)
        check(recovered.returncode == 0, "Exact retained request recovery failed: " + recovered.stderr[-1024:])
        report['recovery'] = json.loads(recovered.stdout)
        report["steps"].append("Recovered the exact retained request without replacing identity or its grant")
    replay = tool("sproozi_run", intent, 3)
    check(replay["id"] == view["id"] and replay["executionId"] == view["executionId"] and
          replay["allocationId"] == view["allocationId"], "Submission replay launched another execution/allocation")
    deadline = time.monotonic() + (600 if args.build_sproozi else 240)
    number = 4
    while time.monotonic() < deadline:
        current = tool("sproozi_status", {"id": view["id"]}, number)
        number += 1
        if current["phase"] == "retained":
            break
        check(current["phase"] not in ("unknown", "cancelled"), "Remote execution needs recovery")
        time.sleep(0.5)
    else:
        raise RuntimeError("Remote task did not retain its checkpoint before timeout")
    check(current["threadId"] and current["result"] == HOST.ANSWERS[5 if args.home_ops else 6 if args.build_sproozi else 0], "Genuine native answer missing")
    if args.build_sproozi:
        check(current['checkpointInfo']['workspace']['revision'] == report['sourceSeed']['revision'],
              "Native task did not retain the actual seeded public source revision")
    report["remote"] = current
    if args.project:
        filtered = tool("sproozi_sessions", {"project": args.project}, number)
        number += 1
        check(any(item['id'] == view['id'] for item in filtered['sessions']), "Project-filtered MCP catalogue omitted work")
        check(all(item.get('project') == args.project for item in filtered['sessions']), "MCP Project filter leaked another scope")
        report['project'] = args.project
    if args.hermes_deployment:
        observed = native_hermes("SPROOZI_WORK_REFERENCE=", {"id": view["id"]}, "hermes-retained-status")
        check(observed == current, "Hermes did not retrieve the actual retained native execution")
        report["steps"].append("Stock Hermes retrieved the completed native session after its sandbox stopped")
    report["steps"].append("Remote initial native task completed and retained while home was absent")
    save()
    with (out / "home.log").open("wb") as log:
        daemon = subprocess.Popen([str(args.session_binary.resolve()), "serve", "--config", str(args.home_config.resolve())],
                                  cwd=ROOT, stdout=log, stderr=log)
        report["homePID"] = daemon.pid
        save()
        try:
            ready = time.monotonic() + 15
            while not socket.exists():
                check(daemon.poll() is None and time.monotonic() < ready, "Home daemon did not start")
                time.sleep(0.1)
            def command(*words):
                completed = subprocess.run([str(args.session_binary.resolve()), *words, "--socket", str(socket)],
                                           cwd=ROOT, capture_output=True, text=True, timeout=120)
                check(completed.returncode == 0, completed.stderr[-1024:])
                return json.loads(completed.stdout)
            pulled = command("pull", view["id"], "--from", source)
            check(pulled["threadId"] == current["threadId"], "Pull changed native identity")
            after = tool("sproozi_status", {"id": view["id"]}, number)
            number += 1
            check(after["phase"] == "home-owned", "Remote owner did not commit exclusive handoff")
            resumed = command("resume", view["id"], "--detach")
            check(resumed["id"] == view["id"] and resumed["threadId"] == current["threadId"], "Resume lost product/native identity")
            check(resumed["executionId"] != view["executionId"] and resumed["allocationId"] != view["allocationId"],
                  "Resume did not create a fresh bounded execution/allocation")
            check(resumed["deadline"] > current["deadline"], "Later local execution did not receive a fresh deadline")
            report["local"] = resumed
            report["steps"].append("Claimed checkpoint resumed in local Docker under a fresh execution identity and deadline")
            save()
            def terminal(index, name):
                with (out / (name + ".stdout")).open("w") as stdout, (out / (name + ".stderr")).open("w") as stderr:
                    subprocess.run(["python3", str(ROOT / "hack/verify/session-mobility/host.py"), "terminal",
                            "--session-binary", str(args.session_binary.resolve()), "--pty-helper", str(args.pty_helper.resolve()),
                            "--socket", str(socket), "--session-id", view["id"], "--output-directory", str(out / name),
                            "--prompt-index", str(index), "--timeout", "540" if args.build_sproozi else "120"], check=True, timeout=600 if args.build_sproozi else 150, cwd=ROOT, stdout=stdout, stderr=stderr)
            terminal(5 if args.home_ops else 6 if args.build_sproozi else 1, "terminal")
            if not args.home_ops and not args.build_sproozi:
                terminal(3, "native-tool")
            stopped = command("stop", view["id"])
            check(stopped["initialStage"] == "retained" and stopped["threadId"] == current["threadId"], "Local follow-up did not retain native state")
            report["stopped"] = stopped
            if args.project:
                local_sessions = command("sessions", "--executor", "docker", "--project", args.project)["sessions"]
                check(any(item['id'] == view['id'] for item in local_sessions), "Project-filtered CLI catalogue omitted local work")
                check(all(item.get('project') == args.project for item in local_sessions), "CLI Project filter leaked another scope")
                check(stopped.get('project') == args.project and stopped.get('repositoryReference') == current.get('repositoryReference'),
                      "Native local continuation lost immutable project/repository association")
            report["steps"].append("Genuine same-conversation local follow-up completed and checkpointed without another writer")
            report["steps"].append("Stock native kubectl observed approved Pod state and visible Secrets denial in both placements"
                                   if args.home_ops else "Actual offline Sproozi package tests passed in both placements" if args.build_sproozi
                                   else "Stock native exec_command wrote and read the controlled workspace file")
            save()
            if args.home_ops:
                report['governance'] = governance.verify(home, profile, args.hermes_kubeconfig, view['id'], out)
            if args.replay_revision:
                fresh_intent = dict(intent, requestId=str(uuid.uuid4()), workspaceFrom=dict(
                    sessionId=view['id'], digest=current['checkpointDigest'], revision=current['checkpointInfo']['workspace']['revision']))
                fresh = native_hermes("SPROOZI_WORK_REQUEST=", fresh_intent, "hermes-recorded-revision") if args.hermes_deployment else tool("sproozi_run", fresh_intent, number)
                number += 1
                check(fresh['id'] != view['id'] and fresh['executionId'] != current['executionId'] and fresh['allocationId'] != current['allocationId'], "Revision launch reused old authority")
                report['revisionExecution'] = fresh
                save()
                deadline = time.monotonic() + (600 if args.build_sproozi else 240)
                while time.monotonic() < deadline:
                    fresh = tool("sproozi_status", {'id': fresh['id']}, number)
                    number += 1
                    if fresh['phase'] == 'retained':
                        break
                    check(fresh['phase'] not in ('unknown', 'cancelled'), "Recorded revision execution needs recovery")
                    time.sleep(0.5)
                else:
                    raise RuntimeError("Recorded revision task did not finish")
                check(fresh['threadId'] != current['threadId'], "Workspace-only task reused native conversation")
                check(fresh['checkpointInfo']['workspace']['revision'] == current['checkpointInfo']['workspace']['revision'],
                      "New task did not use recorded Git HEAD")
                check(fresh['result'] == current['result'], "Recorded revision task lacked genuine native completion")
                source_after = tool("sproozi_status", {'id': view['id']}, number)
                number += 1
                check(source_after['checkpointDigest'] == current['checkpointDigest'] and source_after['phase'] == 'home-owned',
                      "Revision execution mutated source checkpoint or ownership")
                report['revisionExecution'] = fresh
                report['steps'].append('Stock Hermes launched fresh native conversation/authority from exact retained Git revision; source unchanged')
            report["passed"] = True
            save()
        finally:
            try:
                if "local" in report and "stopped" not in report and daemon.poll() is None:
                    operation = str(uuid.uuid4())
                    report["cleanup"] = {"operation": operation}
                    save()
                    cleanup = subprocess.run([str(args.session_binary.resolve()), "cancel", view["id"],
                                              "--socket", str(socket), "--operation", operation],
                                             capture_output=True, text=True, timeout=120, cwd=ROOT)
                    report["cleanup"].update(exit=cleanup.returncode, stderr=cleanup.stderr[-1024:])
                    save()
            finally:
                daemon.terminate()
                try:
                    daemon.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    daemon.kill()
                    daemon.wait()
    print(json.dumps({"passed": report["passed"], "report": str(out / "report.json")}))


if __name__ == "__main__":
    main()
