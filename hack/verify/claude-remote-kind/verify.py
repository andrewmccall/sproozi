#!/usr/bin/env python3
"""Prove Claude Code sessions and tasks the Claude Code remote executor owns.

Drives the public sproozi CLI, through the sproozi-cli context only, against
the Claude Code executor example on a dedicated Kind cluster that run.sh
prepares, with the stock Claude Code client in its terminal through the
production PTY. Each scenario is its own session:

  snapshot   run from the approved repository: the prompt with no dialog, one
             turn, stop (conversation bound, retained, Claude checkpoint),
             resume, the same conversation continues, stop
  upload     run from an uploaded PATH: the prompt with no dialog, a Bash turn
             that reads the uploaded file, then /exit after the turn
  exit       /exit before any turn
  resumed    resume the snapshot session, then /exit
  task       a --task completes and its result is Claude's final reply
  tools      a turn that calls the sproozi diagnose MCP tool and loads a
             profile skill, then stop

The administrator kubeconfig is used only to read the fixture provider's
request log. report.json holds identities, phases and fixture facts.
"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import shutil
import signal
import subprocess
import time
import uuid

HERE = Path(__file__).resolve().parent
ROOT = HERE.parents[2]


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


P = module("claude_remote_provider", HERE / "provider.py")
TUI = module("claude_remote_tui", ROOT / "hack/verify/native-tui.py")
S = P.SCENARIOS

# Text that only a first-run, folder-trust, bypass-permissions or API-key
# dialog shows. The footer's "bypass permissions on" is the normal prompt.
DIALOGS = ["Do you trust the files", "Yes, I accept", "Choose the text style", "Select login method",
           "Let's get started", "Do you want to use this API key", "Yes, proceed"]
# Text the idle prompt shows.
READY = ["? for shortcuts", "bypass permissions on", "for shortcuts"]
DETACH = b"\x1d"
UNSETTLED = {"unknown-stop", "unknown-usage", "failed", "cancelled"}


class Failure(RuntimeError):
    pass


def check(condition, message):
    if not condition:
        raise Failure(message)


class Proof:
    def __init__(self, args):
        self.args = args
        self.out = args.output_directory.resolve()
        self.out.mkdir(parents=True, exist_ok=True, mode=0o700)
        check(not (self.out / "report.json").exists(), "Use a fresh output directory")
        self.home = self.out / "cli-home"
        self.home.mkdir(mode=0o700, exist_ok=True)
        self.env = {"PATH": os.environ["PATH"], "HOME": str(self.home), "LANG": "C.UTF-8",
                    "GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.devnull, "GIT_TERMINAL_PROMPT": "0"}
        TUI.OUT, TUI.HELPER = self.out, args.pty_helper.resolve()
        self.number = 0
        self.report = {"passed": False, "harness": "claude-code", "namespace": args.namespace, "auth": args.auth,
                       "scenarios": {}, "fixtureCredentialsArePublicPlaceholders": True,
                       "limitations": ["Deterministic Anthropic Messages fixture, not the Anthropic service",
                                       "The fixture sign-in is a placeholder; /login itself is not exercised"]}
        self.save()

    def save(self):
        (self.out / "report.json").write_text(json.dumps(self.report, indent=2) + "\n")

    def record(self, scenario, **fields):
        self.report["scenarios"].setdefault(scenario, {}).update(fields)
        self.save()

    # The CLI, through the sproozi-cli context only.

    def remote(self):
        return ["--context", str(self.args.context.resolve()), "--namespace", self.args.namespace]

    def run(self, command, label, timeout=None):
        self.number += 1
        prefix = self.out / ("%02d-%s" % (self.number, label))
        (prefix.with_suffix(".command.json")).write_text(json.dumps(command))
        with prefix.with_suffix(".stdout").open("wb") as stdout, prefix.with_suffix(".stderr").open("wb") as stderr:
            process = subprocess.Popen(command, cwd=self.out, env=self.env, stdin=subprocess.DEVNULL,
                                       stdout=stdout, stderr=stderr, start_new_session=True)
            try:
                process.wait(timeout or self.args.operation_timeout)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
                raise Failure(label + " timed out; see " + str(prefix) + ".stderr")
        check(process.returncode == 0, label + " failed; see " + str(prefix) + ".stderr")
        return prefix.with_suffix(".stdout").read_bytes()

    def cli(self, label, *args, timeout=None):
        command = [str(self.args.sproozi.resolve()), *args, *self.remote(), "--json"]
        return json.loads(self.run(command, label, timeout))

    def start(self, label, path=None, task=None):
        args = ["run", "--remote", "--harness", "claude-code", "--repository", "sproozi",
                "--config", str(self.args.home_config.resolve())]
        if task is not None:
            args += ["--task", task]
        else:
            args += ["-d"]
        if path is not None:
            args.append(str(path))
        view = self.cli(label, *args)
        check(view["harness"] == "claude-code" and view["id"], "The executor did not start a Claude Code session")
        return view

    def status(self, session, label="status"):
        return self.cli(label, "status", session, "--remote")

    def stop(self, session, label):
        view = self.cli(label, "stop", session, "--remote")
        return self.settled(session, view, label)

    def resume(self, session, label):
        return self.cli(label, "resume", session, "--remote", "-d")

    def settled(self, session, view, label, limit=None):
        """Waits for a settled phase; returns the view and the phases seen."""
        seen, began = [view["phase"]], time.monotonic()
        while view["phase"] not in ("retained", *UNSETTLED):
            check(time.monotonic() - began < (limit or self.args.operation_timeout),
                  label + " did not settle; phases " + ", ".join(seen))
            time.sleep(1)
            view = self.status(session, label + "-status")
            if view["phase"] != seen[-1]:
                seen.append(view["phase"])
        check(view["phase"] == "retained", label + " ended " + view["phase"] + ": " + view.get("problem", ""))
        return view, seen, time.monotonic() - began

    # The stock terminal, through sproozi attach.

    def attach(self, session, label):
        command = [str(self.args.sproozi.resolve()), "attach", session, *self.remote()]
        terminal = TUI.Supervisor(command, self.out, {**self.env, "TERM": "xterm-256color",
                                                      "COLORTERM": "truecolor", "LANG": "en_US.UTF-8"},
                                  label, self.out)
        terminal.connect()
        return terminal

    def screen(self, terminal):
        return TUI.clean_terminal(bytes(terminal.data))

    def wait_ready(self, terminal, label):
        """Waits for Claude Code's idle prompt and checks no dialog came first."""
        end = time.monotonic() + self.args.turn_timeout
        while time.monotonic() < end:
            text = self.screen(terminal)
            dialogs = [d for d in DIALOGS if d in text]
            check(not dialogs, label + " showed a dialog: " + ", ".join(dialogs))
            if any(r in text for r in READY):
                time.sleep(1)
                text = self.screen(terminal)
                dialogs = [d for d in DIALOGS if d in text]
                check(not dialogs, label + " showed a dialog: " + ", ".join(dialogs))
                return
            check(terminal.poll() is None, label + ": the attachment ended before the prompt")
            time.sleep(0.2)
        raise Failure(label + " did not reach Claude Code's prompt")

    def turn(self, terminal, scenario, label):
        TUI.submit(TUI.Connection(terminal), S[scenario]["prompt"])
        check(terminal.wait_text(S[scenario]["answer"], self.args.turn_timeout),
              label + " did not render the fixture answer")

    def detach(self, terminal, label):
        terminal.command(DETACH)
        end = time.monotonic() + 15
        while time.monotonic() < end and not terminal.rpc("status").get("exited"):
            time.sleep(0.05)
        check(terminal.rpc("status").get("exited") and terminal.wait(1) == 0, label + " did not detach cleanly")
        terminal.close()

    def exit_claude(self, terminal, session, label):
        """Types /exit and measures how long the executor takes to retain."""
        before = self.status(session, label + "-before")
        terminal.command(b"/exit")
        time.sleep(0.5)
        terminal.command(b"\r")
        began = time.monotonic()
        end = began + self.args.turn_timeout
        while time.monotonic() < end and not terminal.rpc("status").get("exited"):
            time.sleep(0.1)
        check(terminal.rpc("status").get("exited"), label + ": the attachment did not end after /exit")
        terminal.close()
        view, seen, _ = self.settled(session, self.status(session, label + "-after"), label, self.args.exit_timeout)
        elapsed = time.monotonic() - began
        check("unknown-stop" not in seen, label + " passed through unknown-stop")
        check(elapsed <= self.args.exit_timeout, label + " retained after %.1fs" % elapsed)
        return before, view, seen, elapsed

    # The fixture provider's request log, read as the administrator.

    def provider(self):
        output = subprocess.run([str(self.args.kubectl), "--kubeconfig", str(self.args.kubeconfig), "-n",
                                 "sproozi-fixture", "exec", "deploy/claude-fixture", "--", "cat",
                                 "/tmp/fixture/requests.json"], capture_output=True, check=True, timeout=60).stdout
        return json.loads(output)["requests"]

    def conversation(self, scenario):
        return [r for r in self.provider() if r.get("kind") == "conversation" and r["scenario"] == scenario]

    def check_requests(self, scenario, label):
        records = self.conversation(scenario)
        check(records and all(r["completed"] and not r["missingHistory"] for r in records),
              label + ": the provider did not complete the scenario's requests")
        wanted = "sign-in" if self.args.auth == "sign-in" else "key"
        check(all(r["auth"] == wanted for r in records), label + " reached the provider without the " + wanted)
        return records

    # Scenarios.

    def claude_checkpoint(self, view, label):
        """A retained checkpoint of the Claude Code adapter. A task's view
        names the checkpoint's harness; an interactive session's may not, and
        its resumed turn's history then shows the transcript was kept."""
        info = view.get("checkpointInfo")
        check(view.get("checkpointDigest") and view.get("harness") == "claude-code" and
              (info is None or info.get("harness") == "claude-code" and info.get("sha256") == view["checkpointDigest"]),
              label + " retained no Claude checkpoint")

    def snapshot(self):
        view = self.start("snapshot-run")
        session = view["id"]
        self.record("snapshot", sessionId=session, mode=view.get("mode"))
        terminal = self.attach(session, "snapshot-attach")
        self.wait_ready(terminal, "snapshot first visit")
        self.turn(terminal, "first", "snapshot first turn")
        self.detach(terminal, "snapshot first visit")
        stopped, seen, _ = self.stop(session, "snapshot-stop")
        check(stopped.get("threadId"), "The stop did not bind the conversation")
        self.claude_checkpoint(stopped, "snapshot stop")
        self.record("snapshot", noDialog=True, firstStop={"phases": seen, "threadId": stopped["threadId"],
                    "checkpointDigest": stopped["checkpointDigest"]})
        resumed = self.resume(session, "snapshot-resume")
        check(resumed["phase"] not in UNSETTLED, "The resume did not start")
        terminal = self.attach(session, "snapshot-resumed-attach")
        self.wait_ready(terminal, "snapshot resumed visit")
        self.turn(terminal, "resumed", "snapshot resumed turn")
        self.detach(terminal, "snapshot resumed visit")
        again, seen, _ = self.stop(session, "snapshot-second-stop")
        check(again.get("threadId") == stopped["threadId"], "The resumed visit changed the conversation")
        self.claude_checkpoint(again, "snapshot second stop")
        check(again["checkpointDigest"] != stopped["checkpointDigest"], "The resumed turn left the checkpoint unchanged")
        self.check_requests("first", "snapshot first turn")
        self.check_requests("resumed", "snapshot resumed turn")
        self.record("snapshot", passed=True, sameConversation=True,
                    secondStop={"phases": seen, "checkpointDigest": again["checkpointDigest"]})
        return session, again

    def upload(self):
        repository = self.out / "upload-repository"
        repository.mkdir(mode=0o700)
        for command in (["git", "init", "--quiet", "--initial-branch", "upload"],
                        ["git", "config", "user.name", "Fixture"], ["git", "config", "user.email", "fixture@example.invalid"]):
            subprocess.run(command, cwd=repository, env=self.env, check=True, capture_output=True, timeout=30)
        (repository / P.UPLOAD_FILE).write_text(P.UPLOAD_MARKER + "\n")
        for command in (["git", "add", P.UPLOAD_FILE], ["git", "commit", "--quiet", "-m", "Upload fixture"]):
            subprocess.run(command, cwd=repository, env=self.env, check=True, capture_output=True, timeout=30)
        view = self.start("upload-run", path=repository)
        session = view["id"]
        self.record("upload", sessionId=session)
        terminal = self.attach(session, "upload-attach")
        self.wait_ready(terminal, "upload first visit")
        self.turn(terminal, "upload", "upload turn")
        records = self.check_requests("upload", "upload turn")
        check(any(r.get("uploadMarkerSeen") for r in records), "Claude Code's Bash did not read the uploaded file")
        _, retained, seen, elapsed = self.exit_claude(terminal, session, "upload-exit")
        check(not retained.get("threadId"), "A first visit that exited kept a conversation")
        self.record("upload", passed=True, noDialog=True, uploadedFileRead=True,
                    exitAfterTurn={"phases": seen, "seconds": round(elapsed, 1)})

    def exit_first(self):
        view = self.start("exit-run")
        session = view["id"]
        terminal = self.attach(session, "exit-attach")
        self.wait_ready(terminal, "exit first visit")
        _, retained, seen, elapsed = self.exit_claude(terminal, session, "exit-before-turn")
        check(not retained.get("threadId"), "A turnless first visit bound a conversation")
        self.record("exit", sessionId=session, passed=True, noDialog=True,
                    exitBeforeTurn={"phases": seen, "seconds": round(elapsed, 1)})

    def exit_resumed(self, session, stopped):
        self.resume(session, "resumed-exit-resume")
        terminal = self.attach(session, "resumed-exit-attach")
        self.wait_ready(terminal, "resumed visit")
        _, retained, seen, elapsed = self.exit_claude(terminal, session, "resumed-exit")
        check(retained.get("threadId") == stopped["threadId"] and
              retained.get("checkpointDigest") == stopped["checkpointDigest"],
              "An exited resumed visit did not keep the checkpoint it started from")
        self.record("resumed", sessionId=session, passed=True,
                    exitInResumedVisit={"phases": seen, "seconds": round(elapsed, 1)})

    def task(self):
        view = self.start("task-run", task=S["task"]["prompt"])
        session = view["id"]
        check(view.get("mode") in (None, "", "task"), "The task started as an interactive session")
        done, seen, elapsed = self.settled(session, view, "task", self.args.task_timeout)
        self.claude_checkpoint(done, "task")
        check(done.get("threadId"), "The task bound no conversation")
        check(done.get("result") == S["task"]["answer"] and not done.get("resultTruncated"),
              "The task's result is not Claude's final reply")
        records = self.check_requests("task", "task")
        check(len(records) == 1, "The task's prompt was submitted %d times" % len(records))
        self.record("task", sessionId=session, passed=True, phases=seen, seconds=round(elapsed, 1),
                    result=done["result"])

    def tools(self):
        view = self.start("tools-run")
        session = view["id"]
        terminal = self.attach(session, "tools-attach")
        self.wait_ready(terminal, "tools first visit")
        self.turn(terminal, "tools", "tools turn")
        self.detach(terminal, "tools first visit")
        stopped, seen, _ = self.stop(session, "tools-stop")
        self.claude_checkpoint(stopped, "tools stop")
        records = self.check_requests("tools", "tools turn")
        phases = [r["phase"] for r in records]
        final = records[-1]
        check("diagnose" in phases and "skill" in phases and final["phase"] == "answer",
              "The tools turn did not call diagnose and the Skill tool: " + ", ".join(phases))
        check(final.get("diagnoseResult") and final.get("profileSkillLoaded") and not final.get("toolErrors"),
              "The diagnose result or the profile skill did not reach the model")
        self.record("tools", sessionId=session, passed=True, providerPhases=phases, stopPhases=seen,
                    threadId=stopped.get("threadId"))

    def execute(self):
        chosen = self.args.scenario or ["snapshot", "upload", "exit", "resumed", "task", "tools"]
        snapshot = None
        for name in chosen:
            try:
                if name == "snapshot" or name == "resumed" and snapshot is None:
                    snapshot = self.snapshot()
                if name == "upload":
                    self.upload()
                elif name == "exit":
                    self.exit_first()
                elif name == "resumed":
                    self.exit_resumed(*snapshot)
                elif name == "task":
                    self.task()
                elif name == "tools":
                    self.tools()
            except Exception as error:
                self.record(name, passed=False, failure={"type": type(error).__name__, "message": str(error)})
                raise
        unknown = [r for r in self.provider() if r.get("kind") == "refused"]
        self.report.update(passed=True, refusedProviderRequests=len(unknown))
        self.save()


def write_profile(home_config):
    """The profile beside the CLI's configuration, with one skill whose body
    alone carries the marker, so the marker reaches the model only when the
    skill is loaded."""
    skill = home_config.resolve().parent / "profile/skills" / P.PROFILE_SKILL
    skill.mkdir(parents=True, exist_ok=True)
    (skill / "SKILL.md").write_text("---\nname: " + P.PROFILE_SKILL + "\ndescription: Use when asked to load the "
                                    "remote-check profile skill.\n---\n\nThe profile marker is " + P.PROFILE_MARKER + ".\n")


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--sproozi", type=Path, required=True, help="the sproozi CLI built from this checkout")
    parser.add_argument("--pty-helper", type=Path, required=True, help="hack/verify/native-pty, built")
    parser.add_argument("--context", type=Path, required=True, help="kubeconfig holding only the sproozi-cli context")
    parser.add_argument("--home-config", type=Path, required=True,
                        help="the CLI's configuration; its profile folder is written beside it")
    parser.add_argument("--kubeconfig", type=Path, required=True, help="administrator kubeconfig, to read the provider log")
    parser.add_argument("--kubectl", type=Path, default=Path(shutil.which("kubectl") or "kubectl"))
    parser.add_argument("--namespace", default="sproozi-claude-sessions")
    parser.add_argument("--auth", choices=["sign-in", "gateway-key"], default="sign-in")
    parser.add_argument("--output-directory", type=Path, required=True)
    parser.add_argument("--scenario", action="append", choices=["snapshot", "upload", "exit", "resumed", "task", "tools"])
    parser.add_argument("--operation-timeout", type=int, default=240)
    parser.add_argument("--turn-timeout", type=int, default=120)
    parser.add_argument("--task-timeout", type=int, default=300)
    parser.add_argument("--exit-timeout", type=int, default=30,
                        help="seconds from /exit to retained: the executor's 15s exit sweep and the stop")
    args = parser.parse_args()
    write_profile(args.home_config)
    proof = Proof(args)
    try:
        proof.execute()
    except Exception as error:
        proof.report["failure"] = {"type": type(error).__name__, "message": str(error)}
        proof.save()
        raise
    print(json.dumps({"passed": True, "report": str(proof.out / "report.json")}))


if __name__ == "__main__":
    main()
