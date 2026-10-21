#!/usr/bin/env python3
"""Prove `sproozi init` end to end on this computer's Docker engine.

Uses deterministic local providers, an isolated HOME and a short private root,
so it neither spends provider credit nor touches your real installation. It
provisions Codex, adds Claude Code, checks doctor, drives one stock terminal
turn per harness through `sproozi attach`, stops both sessions, proves shared
instructions reach both providers, renews the gateway certificate while
the home daemon runs, and records that a configured kubeconfig and kubectl on
PATH received no calls. The report is written to
.local/verification/local-onboarding/report.json.

Pass --image with a local sha256 content ID or a digest-pinned reference to
skip the image build; otherwise the release bake recipe builds it first.
"""

import argparse
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time

ROOT = Path(__file__).resolve().parents[2]
OUT = ROOT / ".local/verification/local-onboarding"
FIXTURES = ROOT / "hack/verify/session-mobility"
SHARED_MARKER, CODEX_MARKER, CLAUDE_MARKER = "SHARED_PREF_MARKER_42", "CODEX_PREF_MARKER_7", "CLAUDE_PREF_MARKER_9"


def load(name):
    spec = importlib.util.spec_from_file_location(name, FIXTURES / f"{name}.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def write_private(path, text):
    path.write_text(text)
    path.chmod(0o600)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", help="native image content ID or digest-pinned reference")
    args = parser.parse_args()
    OUT.mkdir(parents=True, exist_ok=True)
    work = Path(tempfile.mkdtemp(prefix="spz-", dir="/tmp"))
    work.chmod(0o700)
    root, home, bin_dir = work / "root", work / "home", work / "bin"
    for d in (home, bin_dir):
        d.mkdir(mode=0o700)
    report = {"revision": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
              "dirty": bool(subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT, text=True).strip()),
              "passed": False, "steps": {}}
    providers, installation = [], None
    # A configured cluster and an instrumented kubectl must see no use.
    kube_calls = work / "kubectl-calls"
    write_private(work / "kubeconfig", "apiVersion: v1\nkind: Config\nclusters: []\ncontexts: []\nusers: []\n")
    fake = bin_dir / "kubectl"
    fake.write_text(f"#!/bin/sh\necho \"$@\" >> {kube_calls}\nexit 1\n")
    fake.chmod(0o755)
    env = {"HOME": str(home), "PATH": f"{bin_dir}:{os.environ['PATH']}", "KUBECONFIG": str(work / "kubeconfig"), "LANG": "C.UTF-8"}

    def sproozi(*argv, check=True, timeout=1800):
        result = subprocess.run([str(bin_dir / "sproozi"), *argv], cwd=work, env=env, capture_output=True, text=True, timeout=timeout)
        if check and result.returncode != 0:
            raise RuntimeError(f"sproozi {' '.join(argv)} failed: {result.stderr[-3000:]}")
        return result

    try:
        for target, name in [("./cmd/sproozi", "sproozi"), ("./hack/verify/native-pty", "native-pty")]:
            subprocess.run(["go", "build", "-o", str(bin_dir / name), target], cwd=ROOT, check=True)
        claude = load("claude")
        instructions = claude.INSTRUCTIONS
        write_private(work / "instructions.txt", instructions)
        write_private(work / "codex-token", "tui-placeholder\n")
        write_private(work / "claude-token", claude.TOKEN + "\n")
        provider_dir = work / "claude-provider"
        provider_dir.mkdir(mode=0o700)
        subprocess.run(["openssl", "req", "-x509", "-newkey", "ec", "-pkeyopt", "ec_paramgen_curve:P-256", "-nodes",
                        "-keyout", str(provider_dir / "gateway.key"), "-out", str(provider_dir / "gateway.crt"), "-days", "1",
                        "-subj", "/CN=127.0.0.1", "-addext", "subjectAltName=IP:127.0.0.1"], check=True, capture_output=True)
        shutil.copy(provider_dir / "gateway.crt", work / "claude-ca.pem")
        (work / "claude-ca.pem").chmod(0o600)
        providers.append(subprocess.Popen(["python3", str(FIXTURES / "host.py"), "provider", "--output-directory", str(work / "codex-provider"),
                                           "--listen", "127.0.0.1", "--port", "18765", "--instructions", instructions],
                                          stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL))
        providers.append(subprocess.Popen(["python3", str(FIXTURES / "claude.py"), "serve", "--output-directory", str(provider_dir),
                                           "--listen", "127.0.0.1", "--port", "18775"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL))
        time.sleep(2)
        reference = args.image
        if not reference:
            tag = "docker.io/sproozi-local-verify/native:verify"
            subprocess.run(["docker", "buildx", "bake", "-f", "hack/release/docker-bake.hcl", "native", "--load",
                            "--set", f"native.tags={tag}"], cwd=ROOT, check=True, capture_output=True)
            reference = subprocess.check_output(["docker", "image", "inspect", "--format", "{{.Id}}", tag], text=True).strip()
        report["image"] = reference
        image = ["--image", reference]
        started = time.monotonic()
        result = sproozi("init", "--root", str(root), "--harness", "codex", "--model", "gpt-fixture",
                         "--instructions-file", str(work / "instructions.txt"), *image, "--provider-url", "http://127.0.0.1:18765",
                         "--credential-file", str(work / "codex-token"), "--skip-credential-check", "--json")
        codex = json.loads(result.stdout)
        report["steps"]["initCodex"] = {"ready": codex["ready"], "seconds": round(time.monotonic() - started, 1),
                                        "endpoint": codex["endpoint"], "paths": codex["paths"]}
        installation = codex["installation"]
        result = sproozi("init", "--root", str(root), "--harness", "claude-code", "--model", claude.MODEL,
                         "--provider-url", "https://127.0.0.1:18775", "--provider-ca-file", str(work / "claude-ca.pem"),
                         "--credential-file", str(work / "claude-token"), "--skip-credential-check", "--json")
        both = json.loads(result.stdout)
        report["steps"]["addClaude"] = {"ready": both["ready"], "harnesses": [(h["harness"], h["port"]) for h in both["harnesses"]],
                                        "paths": both["paths"]}
        write_private(root / "profile/AGENTS.md", f"Shared preference {SHARED_MARKER}.\n")
        write_private(root / "profile/codex/AGENTS.md", f"Codex preference {CODEX_MARKER}.\n")
        write_private(root / "profile/claude/CLAUDE.md", f"Claude preference {CLAUDE_MARKER}.\n")
        doctor = json.loads(sproozi("doctor", "--root", str(root), "--json").stdout)
        report["steps"]["doctor"] = {"ok": doctor["ok"], "failed": [c for c in doctor["checks"] if not c["ok"]]}

        repo = work / "repo"
        repo.mkdir()
        for command in (["git", "init", "-q"], ["git", "-c", "user.email=v@e", "-c", "user.name=v", "commit", "-q", "--allow-empty", "-m", "init"]):
            subprocess.run(command, cwd=repo, check=True)
        sessions = {}
        for kind in ("codex", "claude-code"):
            status = json.loads(sproozi("run", str(repo), "--root", str(root), "--harness", kind, "--detach", "--json").stdout)
            sessions[kind] = status["id"]
        socket = root / "state/control.sock"
        turns = {}
        for kind, fixture in (("codex", "host.py"), ("claude-code", "claude.py")):
            drive = subprocess.run(["python3", str(FIXTURES / fixture), "terminal", "--session-binary", str(bin_dir / "sproozi"),
                                    "--pty-helper", str(bin_dir / "native-pty"), "--socket", str(socket),
                                    "--output-directory", str(work / f"terminal-{kind}"), "--session-id", sessions[kind],
                                    "--prompt-index", "0", "--timeout", "120"], cwd=ROOT, env=env, capture_output=True, text=True, timeout=600)
            turns[kind] = json.loads(drive.stdout.strip().splitlines()[-1]).get("passed") is True
        stops = {}
        for kind, session in sessions.items():
            stopped = json.loads(sproozi("stop", session, "--root", str(root), "--json").stdout)
            stops[kind] = {"phase": stopped["phase"], "nativeUUID": bool(stopped.get("threadId"))}
        codex_requests = (work / "codex-provider/requests.json").read_text()
        claude_requests = (provider_dir / "requests.private.json").read_text()
        report["steps"]["sessions"] = {"turns": turns, "stops": stops,
                                       "codexSharedInstructions": all(m in codex_requests for m in (SHARED_MARKER, CODEX_MARKER))
                                       and CLAUDE_MARKER not in codex_requests,
                                       "claudeSharedInstructions": all(m in claude_requests for m in (SHARED_MARKER, CLAUDE_MARKER))
                                       and CODEX_MARKER not in claude_requests,
                                       "consumersReleased": not any((root / "pki/consumers").glob("*.json"))}

        log = root / "logs/service.log"
        pid = (root / "service.pid").read_text().strip()
        before = log.read_text().count("Reloaded gateway serving certificate")
        sproozi("certificates", "renew", "--root", str(root))
        deadline = time.monotonic() + 150
        while time.monotonic() < deadline and log.read_text().count("Reloaded gateway serving certificate") == before:
            time.sleep(5)
        doctor = json.loads(sproozi("doctor", "--root", str(root), "--json").stdout)
        report["steps"]["renewal"] = {"reloaded": log.read_text().count("Reloaded gateway serving certificate") > before,
                                      "samePID": pid == (root / "service.pid").read_text().strip(), "doctorOK": doctor["ok"]}
        stopped = json.loads(sproozi("service", "stop", "--root", str(root), "--json").stdout)
        report["steps"]["serviceStop"] = {"running": stopped["running"]}
        report["steps"]["clusterIsolation"] = {"kubectlCalls": kube_calls.read_text() if kube_calls.exists() else ""}
        s = report["steps"]
        report["passed"] = all([
            s["initCodex"]["ready"], s["addClaude"]["ready"], len(s["addClaude"]["paths"]) == 2, s["doctor"]["ok"],
            all(s["sessions"]["turns"].values()), all(v["phase"] == "retained" and v["nativeUUID"] for v in s["sessions"]["stops"].values()),
            s["sessions"]["codexSharedInstructions"], s["sessions"]["claudeSharedInstructions"], s["sessions"]["consumersReleased"],
            all(s["renewal"].values()), not s["serviceStop"]["running"], s["clusterIsolation"]["kubectlCalls"] == ""])
    finally:
        if installation is not None:
            subprocess.run([str(bin_dir / "sproozi"), "service", "stop", "--root", str(root), "--leave-running"], env=env, capture_output=True)
            subprocess.run(["docker", "network", "rm", "sproozi-" + installation], capture_output=True)
        for p in providers:
            p.terminate()
        (OUT / "report.json").write_text(json.dumps(report, indent=2) + "\n")
        shutil.rmtree(work, ignore_errors=True)
    print(json.dumps({"passed": report["passed"], "report": str(OUT / "report.json")}))
    if not report["passed"]:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
