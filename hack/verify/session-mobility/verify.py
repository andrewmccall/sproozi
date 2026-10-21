#!/usr/bin/env python3
"""Run a credential-free native Docker/remote/Docker round trip.

The administrator starts the deterministic host.py provider, production home
daemon and admitted remote profile separately. This driver manipulates no cluster
resources. Raw command diagnostics, native history and artifacts remain private;
report.json contains only fixture facts, hashes and verified accounting metadata.
This controlled text fixture does not prove tool effects or malicious Git packs.
"""
import argparse
import base64
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import signal
import ssl
import subprocess
import time
import urllib.request
import uuid

ROOT = Path(__file__).resolve().parents[3]
spec = importlib.util.spec_from_file_location("mobility_host", Path(__file__).with_name("host.py"))
HOST = importlib.util.module_from_spec(spec)
spec.loader.exec_module(HOST)


def check(condition, message):
    if not condition:
        raise RuntimeError(message)


def visit_thread(index, visit):
    """Returns a visit's launch thread. Only the first visit may have none: it
    starts from a Git workspace, and the record keeps the thread it bound."""
    thread = visit["launch"]["threadId"]
    check(thread or index == 0, "A later visit launched without the native UUID")
    return thread


def digest(data):
    return hashlib.sha256(data).hexdigest()


PROFILE_SKILL = "mobility-check"
PROFILE_MARKER = re.compile(r"PROFILE_MARKER_[A-Z0-9_]+")


def write_profile(fixture, marker):
    """Writes the user's profile beside the home configuration. Docker reads it
    live at each launch; a move to Kubernetes archives it as it is then."""
    profile = fixture / "profile"
    skill = profile / "skills" / PROFILE_SKILL
    skill.mkdir(parents=True, exist_ok=True)
    for directory in (profile, profile / "skills", skill):
        directory.chmod(0o755)
    for path, text in ((profile / "AGENTS.md", "Profile instruction " + marker + "\n"),
                       (skill / "SKILL.md", "---\nname: " + PROFILE_SKILL + "\ndescription: Profile skill " + marker +
                        "\n---\nConfirm the session carries the profile.\n")):
        path.write_text(text)
        path.chmod(0o644)


def profile_markers(text):
    return sorted(set(PROFILE_MARKER.findall(text)))


class Proof:
    def __init__(self, args):
        self.args = args
        self.out = HOST.private_directory(args.output_directory)
        check(not (self.out / "report.json").exists(), "Use a fresh proof output directory")
        self.config = json.loads(args.home_config.read_text())
        self.fixture = args.home_config.resolve().parent
        check((Path(self.config["gateway"]["providerTokenFile"]).read_text().strip() == HOST.TOKEN),
              "Only the public deterministic fixture credential is permitted")
        hints = json.loads((self.fixture / "profile-hints.json").read_text())
        check(hints["prompts"] == HOST.PROMPTS and hints["answers"] == HOST.TUI.ANSWERS,
              "Provider must be the retained deterministic native fixture")
        self.socket = Path(self.config["directory"]) / "control.sock"
        self.id = str(uuid.uuid4())
        self.env = {"PATH": os.environ["PATH"], "HOME": str(self.out), "LANG": "C.UTF-8",
                    "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull,
                    "GIT_TERMINAL_PROMPT": "0"}
        self.report = {"passed": False, "sessionId": self.id, "fixtureCredentialIsPublicPlaceholder": True,
                       "scope": "real stock native TUI through production session CLI and placement moves",
                       "steps": [], "limitations": ["Text-only history; tool effects and compaction unassessed",
                       "Containment and gateway negative controls are verified separately by the administrator",
                       "Host restore comparison uses a controlled fixture, not a malicious Git pack"]}
        self.number = 0
        self.checkpoint_binary = args.checkpoint_binary
        self.save()

    def save(self):
        HOST.write(self.out / "report.json", self.report)

    def run(self, command, label, cwd=None, data=None, timeout=180):
        self.number += 1
        prefix = self.out / ("%02d-" % self.number + label)
        stdout, stderr = prefix.with_suffix(".stdout"), prefix.with_suffix(".stderr")
        HOST.write(prefix.with_suffix(".command.json"), command)
        with stdout.open("wb") as output, stderr.open("wb") as errors:
            process = subprocess.Popen(command, cwd=cwd or ROOT, env=self.env,
                                       stdin=subprocess.PIPE if data is not None else subprocess.DEVNULL,
                                       stdout=output, stderr=errors, start_new_session=True)
            try:
                process.communicate(data, timeout=timeout)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGTERM)
                try:
                    process.wait(5)
                except subprocess.TimeoutExpired:
                    os.killpg(process.pid, signal.SIGKILL)
                    process.wait()
                raise RuntimeError(label + " timed out; see private command diagnostics")
        check(stdout.stat().st_size <= 10 << 20 and stderr.stat().st_size <= 10 << 20,
              label + " diagnostic output exceeds fixture bound")
        check(process.returncode == 0, label + " failed; see private command diagnostics")
        return stdout.read_bytes()

    def git(self, repository, *args):
        return self.run(["git", "-C", str(repository), *args], "git", timeout=30)

    def seed(self):
        repo = HOST.private_directory(self.out / "source-workspace")
        self.git(repo, "init", "--initial-branch", "mobility-fixture")
        self.git(repo, "config", "user.name", "Mobility fixture")
        self.git(repo, "config", "user.email", "fixture@example.invalid")
        for path, content in {".gitignore": ".env\nneeded.bin\nignored/\n", "staged.txt": "base staged\n",
                              "unstaged.txt": "base unstaged\n", "history.txt": "first commit\n"}.items():
            HOST.write(repo / path, content)
        self.git(repo, "add", ".")
        self.git(repo, "commit", "-m", "Fixture base")
        HOST.write(repo / "history.txt", "second commit\n")
        self.git(repo, "add", "history.txt")
        self.git(repo, "commit", "-m", "Fixture history")
        HOST.write(repo / "staged.txt", "staged work awaiting commit\n")
        self.git(repo, "add", "staged.txt")
        HOST.write(repo / "unstaged.txt", "unstaged work awaiting review\n")
        HOST.write(repo / "untracked.txt", "untracked work awaiting add\n")
        HOST.write(repo / ".env", "PUBLIC_FIXTURE_IGNORED_PLACEHOLDER=excluded\n")
        (repo / "needed.bin").write_bytes(b"public fixture selected ignored binary\x00\xff\n")
        HOST.private_directory(repo / "ignored")
        HOST.write(repo / "ignored/excluded.txt", "ignored fixture bytes\n")
        expected = self.workspace(repo)
        check(expected["stagedDiffBytes"] > 0 and expected["unstagedDiffBytes"] > 0 and
              expected["untracked"] == ["untracked.txt"], "Fixture Git state is not distinct")
        HOST.write(self.out / "expected-workspace.json", expected)
        return repo, expected

    def workspace(self, repo):
        staged = self.git(repo, "diff", "--cached", "--binary", "--no-ext-diff")
        unstaged = self.git(repo, "diff", "--binary", "--no-ext-diff")
        paths = [".gitignore", "staged.txt", "unstaged.txt", "history.txt", "untracked.txt", "needed.bin"]
        return {"head": self.git(repo, "rev-parse", "HEAD").decode().strip(),
                "branch": self.git(repo, "symbolic-ref", "HEAD").decode().strip(),
                "history": self.git(repo, "log", "--format=%H").decode().splitlines(),
                "indexSHA256": digest(self.git(repo, "ls-files", "--stage", "-z")),
                "stagedDiffSHA256": digest(staged), "stagedDiffBytes": len(staged),
                "unstagedDiffSHA256": digest(unstaged), "unstagedDiffBytes": len(unstaged),
                "untracked": sorted(filter(None, self.git(repo, "ls-files", "--others", "--exclude-standard", "-z").decode().split("\0"))),
                "files": {p: digest((repo / p).read_bytes()) for p in paths}}

    def cli(self, action, **flags):
        command = [str(self.args.session_binary.resolve()), action, "--socket", str(self.socket), "--id", self.id, "--json"]
        if action == "run":
            command.append(str(flags.pop("workspace")))
        destination = flags.pop("to", None)
        if destination is not None:
            local = destination in ("local", self.config["local"]["Name"])
            command += ["--local"] if local else ["--remote", "--context", str(destination)]
        for key, value in flags.items():
            for item in value if isinstance(value, list) else [value]:
                command += ["--" + key, str(item)]
        result = json.loads(self.run(command, action, timeout=self.args.operation_timeout))
        HOST.write(self.out / ("status-%02d.json" % self.number), result)
        return result

    def provider(self, quiet=False):
        gateway = self.config["gateway"]
        context = ssl.create_default_context(cafile=gateway.get("providerCAFile"))
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}),
                                            urllib.request.HTTPSHandler(context=context))
        deadline, previous, stable_since = time.monotonic() + 15, None, time.monotonic()
        while True:
            with opener.open(gateway["providerURL"].rstrip("/") + "/status", timeout=5) as response:
                data = response.read((1 << 20) + 1)
                check(len(data) <= 1 << 20, "Provider status exceeds fixture bound")
                requests = json.loads(data)["requests"]
            check(all(not r["missingHistory"] and r["placeholderAuth"] for r in requests),
                  "Actual provider request was unauthorized or missing native history")
            if not quiet:
                return requests
            if requests != previous:
                previous, stable_since = requests, time.monotonic()
            if all(r["completed"] for r in requests) and time.monotonic() - stable_since >= 2:
                return requests
            check(time.monotonic() < deadline, "Fixture provider did not become quiescent")
            time.sleep(0.1)

    def terminal(self, index):
        out = HOST.private_directory(self.out / ("terminal-%d" % index))
        self.run(["python3", str(Path(__file__).with_name("host.py")), "terminal",
                  "--session-binary", str(self.args.session_binary.resolve()),
                  "--pty-helper", str(self.args.pty_helper.resolve()), "--socket", str(self.socket),
                  "--session-id", self.id, "--prompt-index", str(index), "--output-directory", str(out),
                  "--timeout", str(self.args.turn_timeout)], "terminal-%d" % index,
                 timeout=self.args.turn_timeout + 30)
        terminal = json.loads((out / "terminal-evidence.json").read_text())
        check(terminal["passed"] and terminal["cliExitCode"] == 0, "Actual attachment failed")
        requests = self.provider(quiet=True)
        turns = [r["turn"] for r in requests if r["kind"] == "conversation"]
        check(turns == list(range(index + 1)), "Unexpected or duplicate native conversation inference")
        self.report["steps"].append({"turn": index, "attachmentDetached": True,
                                     "completedProviderRequests": len(requests),
                                     "nativeHistoryValidatedInRequest": True})
        self.save()
        return requests

    def validate_record(self, status, deadline, thread=None, placement=None):
        record = status["session"]
        check(status["deadline"] == deadline and record["deadline"] == deadline,
              "Original deadline changed across a move")
        native = record["threadId"]
        check(str(uuid.UUID(native)) == native and (thread is None or native == thread),
              "Actual native UUID changed across a move")
        if placement:
            check(record["phase"] == "active" and record["active"]["launch"]["placement"] == placement,
                  "Destination was not independently activated")
            active = record["active"]["launch"]
            check(active["deadline"] == deadline and active["threadId"] == native and
                  active["allowances"] == record["remaining"] and active.get("includeIgnored") == ["needed.bin"],
                  "Activated destination changed trusted continuity or remaining allocation")
        for index, visit in enumerate(record["visits"]):
            check(visit["actualStopped"] and visit.get("receipt"), "Stopped writer or trusted usage is unknown")
            check(visit["launch"]["deadline"] == deadline and visit_thread(index, visit) in ("", native),
                  "Stopped visit lost original deadline or native UUID")
            check(visit["launch"].get("includeIgnored") == ["needed.bin"],
                  "Stopped visit lost the administrator's ignored-file selection")
            check(visit["receipt"]["launch"] == visit["launch"] and
                  visit["receipt"]["incarnation"] == visit["incarnation"], "Receipt does not bind exact stopped visit")
            check(set(visit["receipt"]["usage"]) == set(record["ceiling"]),
                  "Sealed receipt omitted a granted capability")
        for capability, ceiling in record["ceiling"].items():
            for dimension, unit in [("units", "Units"), ("costMicros", "CostMicros")]:
                if ceiling[dimension]["bounded"]:
                    used = sum(v["receipt"]["usage"][capability][unit] for v in record["visits"])
                    check(used >= 0 and record["remaining"][capability][dimension]["bounded"] and
                          record["remaining"][capability][dimension]["value"] == ceiling[dimension]["value"] - used,
                          "Move did not subtract every sealed capability from original ceiling")
        check(len({v["launch"]["allocationId"] for v in record["visits"]}) == len(record["visits"]),
              "A stopped visit reused an allocation identity")
        return record

    def inspect_checkpoint(self, record, expected, count):
        checkpoint = record["checkpoint"]
        journal = (Path(self.config["directory"]) / "journal").resolve()
        path = (journal / checkpoint["path"]).resolve()
        check(path.is_relative_to(journal), "Checkpoint path escapes trusted journal")
        check(path.stat().st_size <= self.config["local"]["MaxCheckpointBytes"], "Checkpoint exceeds configured bound")
        artifact = path.read_bytes()
        check(digest(artifact) == checkpoint["metadata"]["sha256"], "Committed checkpoint digest differs")
        body = json.loads(artifact)
        native = body["native"] if body["version"] == 1 else body["codex"]
        check(native["ThreadID"] == record["threadId"], "Checkpoint does not contain selected actual native UUID")
        history = base64.b64decode(native["Rollout"], validate=True)
        events = [json.loads(line) for line in history.splitlines() if line]
        complete = [e["payload"]["turn_id"] for e in events if e.get("type") == "event_msg" and
                    e.get("payload", {}).get("type") == "task_complete"]
        check(len(complete) == count and len(set(complete)) == count,
              "Expected genuine completed native turns were not persisted")
        check(complete[-1] == checkpoint["metadata"]["turnId"], "Checkpoint boundary is not final completed turn")
        for answer in HOST.TUI.ANSWERS[:count]:
            check(answer.encode() in history, "Final native history lacks completed fixture answer")
        metas = [e["payload"] for e in events if e.get("type") == "session_meta"]
        check(metas and all(m["id"] == record["threadId"] and bool(m.get("cli_version")) for m in metas),
              "Checkpoint is not the genuine selected native session")
        binary = self.checkpoint_binary
        if binary is None:
            binary = self.out / "checkpoint"
            self.run(["go", "build", "-o", str(binary), "./cmd/checkpoint"], "build-checkpoint")
            self.checkpoint_binary = binary
        restored = self.out / ("restored-turn-%d" % count)
        self.run([str(binary.resolve()), "restore", "--destination", str(restored), "--max-bytes",
                  str(self.config["local"]["MaxCheckpointBytes"])], "restore-final", data=artifact)
        actual = self.workspace(restored / "workspace")
        check(actual == expected, "Final production checkpoint restore changed Git history, index or unfinished files")
        check(not (restored / "workspace/.env").exists() and not (restored / "workspace/ignored").exists(),
              "Unselected ignored files were transferred")
        return {"sha256": digest(artifact), "bytes": len(artifact), "nativeRolloutSHA256": digest(history),
                "completedNativeTurnIds": complete, "nativeVersion": metas[-1]["cli_version"],
                "gitWorkspaceMatches": True, "ignoredFilesExcluded": True, "selectedIgnoredFilePreserved": True,
                "workspaceSnapshotSHA256": digest(json.dumps(actual, sort_keys=True).encode())}

    def execute(self):
        if self.args.finalize_cancel_report:
            previous = json.loads(self.args.finalize_cancel_report.read_text())
            check(not previous["passed"] and previous.get("checkpoint") and
                  [step["turn"] for step in previous["steps"] if "turn" in step] == [0, 1, 2],
                  "Cancellation recovery requires the completed native roundtrip proof")
            self.id = previous["sessionId"]
            self.report = previous
            self.report.pop("failure", None)
            self.report["recoveredCancelReport"] = str(self.args.finalize_cancel_report.resolve())
            deadline, thread = previous["originalDeadline"], previous["threadId"]
            cancelled = self.validate_record(self.cli("recover"), deadline, thread)
            check(cancelled["phase"] == "cancelled" and not cancelled.get("active"),
                  "Recovered cancellation did not seal and stop authority")
            self.finish_cancel(cancelled, deadline, thread)
            return
        if self.args.resume_first_move_report:
            previous = json.loads(self.args.resume_first_move_report.read_text())
            check(not previous["passed"] and len(previous["steps"]) == 1 and previous["steps"][0]["turn"] == 0,
                  "Recovery requires a retained first-turn proof and failed first move")
            self.id = previous["sessionId"]
            self.report["sessionId"] = self.id
            self.report["recoveredFirstMoveReport"] = str(self.args.resume_first_move_report.resolve())
            self.report["steps"] = previous["steps"]
            expected = self.workspace(self.args.resume_first_move_report.parent / "source-workspace")
            deadline = previous["originalDeadline"]
            first = self.validate_record(self.cli("recover"), deadline, placement=self.args.remote_name)
        else:
            check(not self.provider(), "Fixture provider already contains requests; use a fresh provider process")
            repo, expected = self.seed()
            start = self.cli("run", operation=uuid.uuid4(), workspace=repo, **{"include-ignored": ["needed.bin"]})
            check(start["initialStage"] == "active" and not start.get("threadId"),
                  "Initial launch fabricated a native UUID or failed activation")
            deadline = start["deadline"]
            self.report["originalDeadline"] = deadline
            self.save()
            self.terminal(0)
            first = self.validate_record(self.cli("move", operation=uuid.uuid4(), to=self.args.remote_name),
                                         deadline, placement=self.args.remote_name)
        self.report["originalDeadline"] = deadline
        thread = first["threadId"]
        self.report["threadId"] = thread
        self.report["steps"].append({"move": "remote", "epoch": first["epoch"], "stoppedVisits": len(first["visits"]),
                                     "checkpoint": self.inspect_checkpoint(first, expected, 1)})
        self.save()
        self.terminal(1)
        second = self.validate_record(self.cli("move", operation=uuid.uuid4(), to=self.config["local"]["Name"]),
                                      deadline, thread, self.config["local"]["Name"])
        self.report["steps"].append({"move": "local", "epoch": second["epoch"], "stoppedVisits": len(second["visits"]),
                                     "checkpoint": self.inspect_checkpoint(second, expected, 2)})
        self.save()
        requests = self.terminal(2)
        final = self.validate_record(self.cli("move", operation=uuid.uuid4(), to=self.args.remote_name),
                                     deadline, thread, self.args.remote_name)
        check(len(final["visits"]) == 3, "Final checkpoint did not capture all three stopped placements")
        self.report["checkpoint"] = self.inspect_checkpoint(final, expected, 3)
        self.save()
        cancelled = self.validate_record(self.cli("cancel", operation=uuid.uuid4()), deadline, thread)
        check(cancelled["phase"] == "cancelled" and not cancelled.get("active"), "Final cancellation did not stop authority")
        self.finish_cancel(cancelled, deadline, thread)

    def finish_cancel(self, cancelled, deadline, thread):
        requests = self.provider(quiet=True)
        check([r["turn"] for r in requests if r["kind"] == "conversation"] == [0, 1, 2],
              "Final move or cancel caused another conversation inference")
        receipts = []
        used = 0
        for visit in cancelled["visits"]:
            usage = visit["receipt"]["usage"]["model.inference"]
            check(usage["Units"] >= 0 and usage["CostMicros"] == 0, "Fixture receipt usage is invalid")
            used += usage["Units"]
            receipts.append({"placement": visit["launch"]["placement"], "epoch": visit["launch"]["epoch"],
                             "allocationId": visit["launch"]["allocationId"], "incarnation": visit["incarnation"],
                             "actualStopped": True, "usage": usage})
        check(used == 15 * len(requests), "Sealed usage differs from actual conversation and title SSE usage")
        ceiling = cancelled["ceiling"]["model.inference"]["units"]
        remaining = cancelled["remaining"]["model.inference"]["units"]
        check(ceiling["bounded"] and remaining["bounded"] and remaining["value"] == ceiling["value"] - used,
              "Remaining allowance did not subtract immutable sealed usage")
        check(all(v["launch"]["threadId"] == thread for v in cancelled["visits"]), "Native UUID changed")
        self.report.update(passed=True, finalPhase="cancelled", sameNativeUUID=True, sameOriginalDeadline=True,
                           visits=receipts, providerRequests={"conversation": 3,
                           "title": len(requests) - 3, "completed": len(requests), "tokensPerRequest": 15},
                           accounting={"ceilingUnits": ceiling["value"], "sealedUnits": used,
                                       "remainingUnits": remaining["value"], "matchesActualSSEUsage": True})
        self.save()


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-directory", type=Path, required=True)
    parser.add_argument("--home-config", type=Path, required=True)
    parser.add_argument("--session-binary", type=Path, required=True)
    parser.add_argument("--pty-helper", type=Path, required=True)
    parser.add_argument("--checkpoint-binary", type=Path)
    parser.add_argument("--remote-name", default="remote")
    parser.add_argument("--operation-timeout", type=int, default=240)
    parser.add_argument("--turn-timeout", type=int, default=60)
    parser.add_argument("--profile-server-log", type=Path)
    parser.add_argument("--home-log", type=Path)
    recovery = parser.add_mutually_exclusive_group()
    recovery.add_argument("--finalize-cancel-report", type=Path,
                          help="replay retained cancellation after the recorded complete roundtrip")
    recovery.add_argument("--resume-first-move-report", type=Path,
                        help="recover the exact retained first move after an independently corrected admission failure")
    args = parser.parse_args()
    proof = Proof(args)
    try:
        proof.execute()
    except Exception as error:
        proof.report["failure"] = str(error)
        # Logs are private and bounded; paths/content never enter the public report.
        for name, path in [("profile", args.profile_server_log), ("home", args.home_log)]:
            if path and path.is_file():
                with path.open("rb") as source:
                    source.seek(max(0, path.stat().st_size - (64 << 10)))
                    (proof.out / (name + "-failure-tail.log")).write_bytes(source.read(64 << 10))
        proof.save()
        raise
    print(json.dumps({"passed": True, "report": str(proof.out / "report.json"),
                      "sessionId": proof.id, "threadId": proof.report["threadId"]}))


if __name__ == "__main__":
    main()
