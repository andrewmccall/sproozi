#!/usr/bin/env python3
"""Prove genuine native tool effects, graceful held-turn move and forced cancel.

The administrator starts the fixture provider/home/profile and supplies a dedicated
Kind kubeconfig. Kubernetes probes are read-only except the fixed native status
command. Raw native history and administrator probes remain in private output.
"""
import argparse
import base64
import importlib.util
import json
import os
from pathlib import Path
import signal
import subprocess
import time
import uuid

SPEC = importlib.util.spec_from_file_location("mobility_roundtrip", Path(__file__).with_name("verify.py"))
BASE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(BASE)
HOST, check, digest = BASE.HOST, BASE.check, BASE.digest


def pending_source(record, active, checkpoint, operation):
    if record.get("pendingOperation") != operation or record["phase"] != "stopping":
        return False
    check(record["active"] == active and record["checkpoint"] == checkpoint,
          "Pending move changed source or checkpoint before safe boundary")
    intent = next(op for op in record["operations"] if op["id"] == operation)
    check(intent["stage"] == "stop" and not intent.get("launch") and not intent.get("incarnation"),
          "Destination prepared before actual source turn completed")
    return True


def completed_turns(events, prefix, count, boundary):
    complete = [event["payload"]["turn_id"] for event in events if event.get("type") == "event_msg" and
                event.get("payload", {}).get("type") == "task_complete"]
    check(len(complete) == count and len(set(complete)) == len(complete) and
          complete[:len(prefix)] == prefix and complete[-1] == boundary,
          "Exact native completed turn IDs/boundary changed")
    return complete


class Background:
    def __init__(self, proof, command, label):
        proof.number += 1
        self.prefix = proof.out / ("%02d-" % proof.number + label)
        HOST.write(self.prefix.with_suffix(".command.json"), command)
        self.stdout_path, self.stderr_path = self.prefix.with_suffix(".stdout"), self.prefix.with_suffix(".stderr")
        self.stdout = self.stdout_path.open("wb")
        self.stderr = self.stderr_path.open("wb")
        self.process = subprocess.Popen(command, cwd=BASE.ROOT, env=proof.env, stdin=subprocess.DEVNULL,
                                        stdout=self.stdout, stderr=self.stderr, start_new_session=True)

    def finish(self, timeout, success=True):
        try:
            code = self.process.wait(timeout)
        except subprocess.TimeoutExpired:
            raise RuntimeError("Background command did not finish; private diagnostics retained")
        self.stdout.close()
        self.stderr.close()
        check(self.stdout_path.stat().st_size <= 10 << 20 and self.stderr_path.stat().st_size <= 10 << 20,
              "Background diagnostics exceed fixture bound")
        if success:
            check(code == 0, "Background command failed; private diagnostics retained")
        return code, self.stdout_path.read_bytes()

    def close(self):
        if self.process.poll() is None:
            os.killpg(self.process.pid, signal.SIGTERM)
            try:
                self.process.wait(5)
            except subprocess.TimeoutExpired:
                os.killpg(self.process.pid, signal.SIGKILL)
                self.process.wait()
        self.stdout.close()
        self.stderr.close()


class ExtendedProof(BASE.Proof):
    def __init__(self, args):
        super().__init__(args)
        self.provider_directory = HOST.private_directory(args.provider_directory)
        check((self.provider_directory / "provider-listener.json").is_file(), "Require the actual fixture provider directory")
        self.backgrounds, self.completed_ids = [], []
        self.report["scope"] = "genuine native tool execution, held graceful move, exact forced cancellation and accounting replay"
        self.report["limitations"] = ["Controlled exec_command file effect; arbitrary tools and compaction unassessed",
            "Dedicated Kind administrator probes confirm exact resource UIDs; hostile cluster administrators unassessed",
            "Socket flush is provider completion, separate from native task_complete and accounting seal"]
        self.save()

    def command(self, action, **flags):
        command = [str(self.args.session_binary.resolve()), action, "--socket", str(self.socket), "--id", self.id, "--json"]
        destination = flags.pop("to", None)
        if destination is not None:
            local = destination in ("local", self.config["local"]["Name"])
            # A remote destination is reached through the kubeconfig context,
            # by default the "sproozi" context; destination is the executor's name.
            context = ["--context", str(self.args.context)] if getattr(self.args, "context", None) else []
            command += ["--local"] if local else ["--remote", *context]
        for key, value in flags.items():
            command += ["--" + key, str(value)]
        return command

    def background(self, command, label):
        child = Background(self, command, label)
        self.backgrounds.append(child)
        return child

    def wait(self, predicate, label, timeout=None):
        deadline = time.monotonic() + (timeout or self.args.observe_timeout)
        while True:
            result = predicate()
            if result:
                return result
            check(time.monotonic() < deadline, label + " was not independently observed")
            time.sleep(0.1)

    def journal(self):
        path = Path(self.config["directory"]) / "journal" / (self.id + ".json")
        check(path.stat().st_size <= 1 << 20, "Journal exceeds fixture bound")
        return json.loads(path.read_text())

    def docker_records(self):
        records = []
        for path in (Path(self.config["directory"]) / "docker").glob("*.json"):
            check(path.stat().st_size <= 1 << 20, "Docker placement record exceeds bound")
            record = json.loads(path.read_text())
            if record.get("launch", {}).get("sessionId") == self.id:
                records.append(record)
        return sorted(records, key=lambda record: record["launch"]["epoch"])

    def docker(self, *arguments):
        command = [self.config["dockerBinary"], "--host", "unix://" + self.config["dockerSocket"],
                   "--config", self.config["dockerConfig"], *arguments]
        return self.run(command, "docker-probe", timeout=35)

    def container(self, instance):
        data = json.loads(self.docker("container", "inspect", instance["ID"]))
        check(len(data) == 1 and data[0]["Id"] == instance["ID"] and
              data[0]["Config"]["Labels"]["sproozi.com/execution"] == instance["Execution"],
              "Actual Docker incarnation changed")
        return data[0]

    def writer_stopped(self, record, role="Agent"):
        """Checks a stopped visit's container: absent once the placement recorded
        its removal, otherwise present and not running."""
        if record.get("removed"):
            check(not self.docker("container", "ls", "--all", "--quiet", "--filter", "id=" + record[role]["ID"]).strip(),
                  "Docker placement recorded removal but its container remains")
            return
        check(not self.container(record[role])["State"]["Running"], "Stopped Docker writer remains running")

    def kube(self, namespace, *arguments):
        return self.run([str(self.args.kubectl), "--kubeconfig", str(self.args.kubeconfig.resolve()),
                         "--namespace", namespace, *arguments], "kube-probe", timeout=35)

    def registries(self):
        objects = json.loads(self.kube(self.args.namespace, "get", "configmaps", "-l",
                                     "sessions.sproozi.com/registry=true", "-o", "json"))["items"]
        records = []
        for obj in objects:
            raw = obj.get("data", {}).get("registration.json")
            if raw:
                record = json.loads(raw)
                if record["registration"]["sessionId"] == self.id:
                    records.append({"uid": obj["metadata"]["uid"], "name": obj["metadata"]["name"], **record})
        return sorted(records, key=lambda record: record["registration"]["epoch"])

    def no_destination_writer(self, destination):
        if destination == self.config["local"]["Name"]:
            for record in self.docker_records():
                if record.get("Agent"):
                    self.writer_stopped(record)
            return
        records = self.registries()
        check(all(record["phase"] in ["stopped", "cancelled"] for record in records),
              "Remote destination retains an admitted allocation")
        uids = {record["podUid"] for record in records if record["podUid"]}
        pods = json.loads(self.kube(self.args.agent_namespace, "get", "pods", "-o", "json"))["items"]
        for pod in pods:
            if pod["metadata"]["uid"] in uids:
                check(all("running" not in container["state"] for container in pod["status"].get("containerStatuses", [])
                          if container["name"] == "agent"), "Existing remote destination native writer is running")

    def native_source(self, active):
        if active["launch"]["placement"] == self.config["local"]["Name"]:
            records = [record for record in self.docker_records() if record["launch"] == active["launch"]]
            check(len(records) == 1, "Local source placement registration absent")
            agent = records[0]["Agent"]
            check(agent["ID"] == active["incarnation"]["resourceUid"] and self.container(agent)["State"]["Running"],
                  "Exact local source native container was not running")
            status = json.loads(self.docker("exec", agent["ID"], "/usr/local/bin/native-session", "status"))
        else:
            records = [record for record in self.registries() if record["runUid"] == active["incarnation"]["executionId"]]
            check(len(records) == 1 and records[0]["podUid"] == active["incarnation"]["resourceUid"],
                  "Remote registration changed exact source incarnation")
            pods = json.loads(self.kube(self.args.agent_namespace, "get", "pods", "-o", "json"))["items"]
            matches = [pod for pod in pods if pod["metadata"]["uid"] == records[0]["podUid"]]
            check(len(matches) == 1, "Exact remote source Pod is unavailable")
            pod = matches[0]
            agents = [container for container in pod["status"].get("containerStatuses", []) if container["name"] == "agent"]
            check(len(agents) == 1 and "running" in agents[0]["state"], "Actual remote native container was not running")
            status = json.loads(self.kube(self.args.agent_namespace, "exec", pod["metadata"]["name"], "-c", "agent",
                                         "--", "/usr/local/bin/native-session", "status"))
        check(not status.get("error") and status.get("threadId") == active["launch"]["threadId"],
              "Native source status lost actual UUID")
        return status

    def rendered_profile(self, active, marker):
        """Checks the profile Codex received in the exact source container: its
        instructions follow the Sproozi text and its skill sits in Codex's home.
        A Kubernetes visit must also name the stored archive in its registration."""
        home = "/workspace/session/codex/"
        command = ["cat", home + "AGENTS.md", home + "skills/" + BASE.PROFILE_SKILL + "/SKILL.md"]
        if active["launch"]["placement"] == self.config["local"]["Name"]:
            records = [record for record in self.docker_records() if record["launch"] == active["launch"]]
            check(len(records) == 1, "Local profile probe requires one exact source")
            rendered = self.docker("exec", records[0]["Agent"]["ID"], *command)
        else:
            records = [record for record in self.registries() if record["runUid"] == active["incarnation"]["executionId"]]
            check(len(records) == 1 and records[0]["registration"].get("profileDigest", "").startswith("sha256:"),
                  "Remote registration does not name the stored profile")
            pods = [pod for pod in json.loads(self.kube(self.args.agent_namespace, "get", "pods", "-o", "json"))["items"]
                    if pod["metadata"]["uid"] == records[0]["podUid"]]
            check(len(pods) == 1, "Exact remote source Pod is unavailable for the profile probe")
            rendered = self.kube(self.args.agent_namespace, "exec", pods[0]["metadata"]["name"], "-c", "agent", "--", *command)
        rendered = rendered.decode()
        check("Sproozi managed environment" in rendered and BASE.profile_markers(rendered) == [marker] and
              rendered.index("Sproozi managed environment") < rendered.index(marker),
              "Codex did not receive the profile as it was when this visit began")
        self.report["steps"].append({"profile": active["launch"]["placement"], "profileMarker": marker})
        self.save()

    def denials(self, active=None):
        source = Path(__file__).with_name("native-denials.js").read_text()
        if active is None or active["launch"]["placement"] == self.config["local"]["Name"]:
            records = self.docker_records()
            if active:
                records = [record for record in records if record["launch"] == active["launch"]]
            check(len(records) == 1, "Denial probe requires one exact local source")
            agent = records[0]["Agent"]
            check(self.container(agent)["State"]["Running"], "Denial probe source is not actually running")
            if active:
                check(agent["ID"] == active["incarnation"]["resourceUid"], "Denial probe source UID changed")
            observed = json.loads(self.docker("exec", "--user", "1000", agent["ID"], "/usr/local/bin/node", "-e", source))
            placement = self.config["local"]["Name"]
        else:
            self.native_source(active)  # Confirms exact registry/Pod UID and running native container.
            pods = json.loads(self.kube(self.args.agent_namespace, "get", "pods", "-o", "json"))["items"]
            matches = [pod for pod in pods if pod["metadata"]["uid"] == active["incarnation"]["resourceUid"]]
            check(len(matches) == 1, "Denial probe remote Pod UID changed")
            observed = json.loads(self.kube(self.args.agent_namespace, "exec", matches[0]["metadata"]["name"],
                                           "-c", "agent", "--", "/usr/local/bin/node", "-e", source))
            placement = active["launch"]["placement"]
        check(observed["passed"] and observed["nativeUID"] == 1000, "Actual native worker denials failed")
        self.report["steps"].append({"nativeDenialsPlacement": placement, "observed": observed})
        self.save()

    def terminal_command(self, index, label):
        out = HOST.private_directory(self.out / label)
        return ["python3", str(Path(__file__).with_name("host.py")), "terminal", "--session-binary",
                str(self.args.session_binary.resolve()), "--pty-helper", str(self.args.pty_helper.resolve()),
                "--socket", str(self.socket), "--session-id", self.id, "--prompt-index", str(index),
                "--output-directory", str(out), "--timeout", str(self.args.turn_timeout)], out

    def terminal(self, index):
        label = "terminal-" + str(index)
        command, out = self.terminal_command(index, label)
        self.run(command, label, timeout=self.args.turn_timeout + 30)
        evidence = json.loads((out / "terminal-evidence.json").read_text())
        check(evidence["passed"] and evidence["cliExitCode"] == 0, "Actual native attachment did not complete")
        requests = self.provider(quiet=True)
        if index == 3:
            calls = [request for request in requests if request["turn"] == 3 and request["phase"] == "tool-call"]
            results = [request for request in requests if request["turn"] == 3 and request["kind"] == "tool-followup"]
            check(len(calls) == len(results) == 1 and results[0]["toolOutputValidated"], "Genuine native tool followup absent")
        self.report["steps"].append({"turn": index, "attachmentDetached": True,
                                     "completedProviderRequests": len(requests)})
        self.save()

    def workspace(self, repo):
        snapshot = super().workspace(repo)
        effect = repo / HOST.TOOL_FILE
        if effect.exists():
            snapshot["files"][HOST.TOOL_FILE] = digest(effect.read_bytes())
        return snapshot

    def inspect_checkpoint(self, record, expected, answers):
        checkpoint = record["checkpoint"]
        journal = (Path(self.config["directory"]) / "journal").resolve()
        path = (journal / checkpoint["path"]).resolve()
        check(path.is_relative_to(journal) and path.stat().st_size <= self.config["local"]["MaxCheckpointBytes"],
              "Checkpoint exceeds trusted path/size boundary")
        artifact = path.read_bytes()
        check(digest(artifact) == checkpoint["metadata"]["sha256"], "Checkpoint digest changed")
        body = json.loads(artifact)
        native = body["native"] if body["version"] == 1 else body["codex"]
        check(body["version"] == 1 or body.get("harness") == "codex", "Checkpoint client kind changed")
        check(native["ThreadID"] == record["threadId"], "Native checkpoint UUID changed")
        history = base64.b64decode(native["Rollout"], validate=True)
        events = [json.loads(line) for line in history.splitlines() if line]
        complete = completed_turns(events, self.completed_ids, len(answers), checkpoint["metadata"]["turnId"])
        check(all(answer.encode() in history for answer in answers), "Persisted native answers absent")
        metas = [event["payload"] for event in events if event.get("type") == "session_meta"]
        check(metas and all(meta["id"] == record["threadId"] and bool(meta.get("cli_version")) for meta in metas),
              "Actual stock client identity changed")
        if HOST.ANSWERS[3] in answers:
            check(b'custom_tool_call' in history and b'custom_tool_call_output' in history and
                  HOST.TOOL_MARKER.encode() in history and b'exec_command' in history, "Real native tool history absent")
        self.completed_ids = complete
        if self.checkpoint_binary is None:
            self.checkpoint_binary = self.out / "checkpoint"
            self.run(["go", "build", "-o", str(self.checkpoint_binary), "./cmd/checkpoint"], "build-checkpoint")
        destination = self.out / ("restored-" + str(self.number) + "-" + str(len(complete)))
        self.run([str(self.checkpoint_binary.resolve()), "restore", "--destination", str(destination), "--max-bytes",
                  str(self.config["local"]["MaxCheckpointBytes"])], "restore-checkpoint", data=artifact)
        actual = self.workspace(destination / "workspace")
        check(actual == expected, "Tool effect or Git history/index/staged/unstaged/ignored state changed")
        check(not (destination / "workspace/.env").exists() and not (destination / "workspace/ignored").exists(),
              "Unselected ignored files transferred")
        return {"sha256": digest(artifact), "completedNativeTurnIds": complete, "nativeRolloutSHA256": digest(history),
                "gitWorkspaceMatches": True, "toolFileSHA256": actual["files"].get(HOST.TOOL_FILE),
                "selectedIgnoredFilePreserved": True}

    def arm(self):
        return json.loads(self.run(["python3", str(Path(__file__).with_name("host.py")), "control",
                    "--output-directory", str(self.provider_directory), "--operation", "arm", "--prompt-index", "4"], "arm"))["id"]

    def release(self, hold_id):
        self.run(["python3", str(Path(__file__).with_name("host.py")), "control", "--output-directory",
                  str(self.provider_directory), "--operation", "release", "--hold-id", hold_id], "release")

    def held_move(self, destination, cancel=False):
        source = self.journal()
        check(source["phase"] == "active", "Held test requires actual active source")
        active, checkpoint = source["active"], source["checkpoint"]
        docker_before = [record["launch"]["allocationId"] for record in self.docker_records()]
        registry_before = [record["uid"] for record in self.registries()]
        hold_id = self.arm()
        command, terminal_out = self.terminal_command(4, "terminal-held-" + ("cancel" if cancel else "move"))
        terminal = self.background(command, "held-terminal")
        self.wait(lambda: next((request for request in self.provider() if request.get("holdID") == hold_id
                                      and request.get("held") and not request["completed"]), None), "Actual held provider request")
        operation = str(uuid.uuid4())
        move = self.background(self.command("move", operation=operation, to=destination), "held-move")
        def pending():
            record = self.journal()
            if not pending_source(record, active, checkpoint, operation):
                return None
            status = self.native_source(active)
            return record if status["state"] == "stopping" else None
        retained = self.wait(pending, "Durable source stop intent and actual native input gate")
        check(move.process.poll() is None, "Move returned while provider reply was held")
        check([record["launch"]["allocationId"] for record in self.docker_records()] == docker_before and
              [record["uid"] for record in self.registries()] == registry_before,
              "Held source admitted a destination allocation")
        self.no_destination_writer(destination)
        HOST.write(self.out / ("pending-cancel.json" if cancel else "pending-move.json"), retained)
        self.report["steps"].append({"heldTurn": 4, "forcedCancel": cancel, "sourcePlacement": active["launch"]["placement"],
            "durablePendingSource": True, "actualNativeInputGated": True, "destinationNotPrepared": True})
        self.save()
        if not cancel:
            self.release(hold_id)
            _, raw = move.finish(self.args.operation_timeout)
            terminal.finish(self.args.turn_timeout + 10)
            evidence = json.loads((terminal_out / "terminal-evidence.json").read_text())
            check(evidence["passed"], "Released native held turn did not actually complete")
            self.provider(quiet=True)
            return json.loads(raw)["session"]
        before_cancel = time.monotonic()
        cancellation = self.background(self.command("cancel", operation=uuid.uuid4()), "forced-cancel")
        _, raw = cancellation.finish(self.args.cancel_bound)
        cancelled = json.loads(raw)["session"]
        elapsed = time.monotonic() - before_cancel
        check(elapsed < self.args.cancel_bound and cancelled["phase"] == "cancelled" and not cancelled.get("active"),
              "Forced cancellation did not stop exact source within bound")
        check(cancelled["checkpoint"] == checkpoint, "Forced cancellation invented a new completed checkpoint")
        check(move.finish(10, success=False)[0] != 0, "Cancelled graceful Move falsely returned success")
        records = [record for record in self.docker_records() if record["launch"] == active["launch"]]
        check(len(records) == 1 and records[0]["actualStopped"], "Local cancellation lacks actual stopped observation")
        check(records[0].get("removed"), "Cancelled Docker visit kept its containers")
        for role in ["Agent", "Helper", "Anchor"]:
            self.writer_stopped(records[0], role)
        check([record["uid"] for record in self.registries()] == registry_before,
              "Forced cancelled move created a remote destination")
        self.no_destination_writer(destination)
        check(all(visit["actualStopped"] and visit.get("receipt") for visit in cancelled["visits"]),
              "Cancellation lost known conservative accounting")
        check(cancelled["visits"][-1]["launch"] == active["launch"] and
              cancelled["visits"][-1]["incarnation"] == active["incarnation"], "Cancellation sealed a different source")
        final_usage = cancelled["visits"][-1]["receipt"]["usage"]["model.inference"]["Units"]
        check(final_usage > 0, "Pending inference was incorrectly sealed as zero")
        self.report["steps"].append({"cancelBoundSeconds": self.args.cancel_bound, "observedCancelSeconds": elapsed,
                                     "exactAgentHelperAnchorStopped": True, "noDestinationCreated": True})
        # Release old upstream only after the independent writer fence. Its
        # eventual response or disconnect may settle accounting but cannot refund.
        self.release(hold_id)
        self.wait(lambda: next((request for request in self.provider() if request.get("holdID") == hold_id and
                               (request.get("completed") or request.get("streamError"))), None), "Old upstream completion/disconnect")
        replay = self.validate_record(self.cli("recover"), self.report["originalDeadline"], self.report["threadId"])
        check(replay["visits"] == cancelled["visits"] and replay["remaining"] == cancelled["remaining"] and
              replay["checkpoint"] == checkpoint and not replay.get("active"), "Released old response amended sealed cancellation")
        terminal.close()
        return replay

    def execute(self):
        remote = None
        if self.args.resume_first_move_report:
            previous = json.loads(self.args.resume_first_move_report.read_text())
            check(not previous["passed"] and [step["turn"] for step in previous["steps"] if "turn" in step] == [0]
                  and not previous.get("firstCheckpoint"),
                  "First move recovery requires the retained single completed local turn")
            self.id, self.report = previous["sessionId"], previous
            self.report.pop("failure", None)
            self.report["recoveredFirstMoveReport"] = str(self.args.resume_first_move_report.resolve())
            deadline = previous["originalDeadline"]
            remote = self.validate_record(self.cli("status"), deadline, previous.get("threadId"), self.args.remote_name)
            check(remote["epoch"] == 2 and len(remote["visits"]) == 1,
                  "Retained first move has additional visits")
            expected = json.loads((self.args.resume_first_move_report.parent / "expected-workspace.json").read_text())
            check([r["turn"] for r in self.provider() if r["kind"] == "conversation"] == [0],
                  "Retained first turn census changed")
        elif self.args.resume_initial_report:
            previous = json.loads(self.args.resume_initial_report.read_text())
            check(not previous["passed"] and [step["turn"] for step in previous["steps"] if "turn" in step] == [0]
                  and not previous.get("firstCheckpoint"), "Initial recovery requires exactly the completed local first turn")
            self.id = previous["sessionId"]
            self.report = previous
            self.report.pop("failure", None)
            self.report["recoveredInitialReport"] = str(self.args.resume_initial_report.resolve())
            deadline = previous["originalDeadline"]
            status = self.cli("status")
            check(status["deadline"] == deadline and status["initialStage"] == "active", "Retained initial visit changed")
            expected = json.loads((self.args.resume_initial_report.parent / "expected-workspace.json").read_text())
            check([r["turn"] for r in self.provider() if r["kind"] == "conversation"] == [0], "Retained first turn census changed")
        else:
            check(not self.provider(), "Use a fresh deterministic provider process")
            BASE.write_profile(self.fixture, "PROFILE_MARKER_DOCKER_FIRST")
            repo, expected = self.seed()
            start = self.cli("run", operation=uuid.uuid4(), workspace=repo, **{"include-ignored": ["needed.bin"]})
            check(start["initialStage"] == "active" and not start.get("threadId"), "Initial native UUID fabricated")
            deadline = start["deadline"]
            self.report["originalDeadline"] = deadline
            self.save()
            self.terminal(0)
            self.rendered_profile(self.cli("status")["session"]["active"], "PROFILE_MARKER_DOCKER_FIRST")
            BASE.write_profile(self.fixture, "PROFILE_MARKER_KUBERNETES")
        if remote is None:
            self.denials()
            remote = self.validate_record(self.cli("move", operation=uuid.uuid4(), to=self.args.remote_name), deadline,
                                          placement=self.args.remote_name)
        thread = remote["threadId"]
        self.report["threadId"] = thread
        self.report["firstCheckpoint"] = self.inspect_checkpoint(remote, expected, [HOST.ANSWERS[0]])
        self.save()
        self.terminal(1)
        if not (self.args.resume_first_move_report or self.args.resume_initial_report):
            self.rendered_profile(remote["active"], "PROFILE_MARKER_KUBERNETES")
            BASE.write_profile(self.fixture, "PROFILE_MARKER_DOCKER_EDITED")
        self.denials(remote["active"])
        self.terminal(3)
        expected["untracked"] = sorted([*expected["untracked"], HOST.TOOL_FILE])
        expected["files"][HOST.TOOL_FILE] = digest((HOST.TOOL_MARKER + "\n").encode())
        local = self.held_move(self.config["local"]["Name"])
        local = self.validate_record({"session": local, "deadline": deadline}, deadline, thread, self.config["local"]["Name"])
        answers = [HOST.ANSWERS[index] for index in [0, 1, 3, 4]]
        self.report["toolCheckpoint"] = self.inspect_checkpoint(local, expected, answers)
        self.save()
        self.terminal(2)
        if not (self.args.resume_first_move_report or self.args.resume_initial_report):
            self.rendered_profile(local["active"], "PROFILE_MARKER_DOCKER_EDITED")
        # Capture the completed local turn, then return idle to a fresh local
        # allocation for the cancellation test. Neither idle move infers again.
        remote = self.validate_record(self.cli("move", operation=uuid.uuid4(), to=self.args.remote_name), deadline, thread, self.args.remote_name)
        answers.append(HOST.ANSWERS[2])
        self.report["finalCompletedCheckpoint"] = self.inspect_checkpoint(remote, expected, answers)
        local = self.validate_record(self.cli("move", operation=uuid.uuid4(), to=self.config["local"]["Name"]), deadline, thread, self.config["local"]["Name"])
        # Native resume can append metadata before any user turn. Validate exact
        # completed-turn identities and workspace, not byte-identical artifacts.
        idle = self.inspect_checkpoint(local, expected, answers)
        check(idle["completedNativeTurnIds"] == self.report["finalCompletedCheckpoint"]["completedNativeTurnIds"],
              "Idle placement changed completed native turns")
        self.report["idleCheckpoint"] = idle
        self.save()
        cancelled = self.held_move(self.args.remote_name, cancel=True)
        self.validate_record({"session": cancelled, "deadline": deadline}, deadline, thread)
        requests = self.provider()
        completed_usage = sum(request.get("usage", {}).get("total_tokens", 0) for request in requests if request["completed"])
        sealed = sum(visit["receipt"]["usage"]["model.inference"]["Units"] for visit in cancelled["visits"])
        check(sealed >= completed_usage and all(BASE.visit_thread(i, v) in ("", thread) for i, v in enumerate(cancelled["visits"])),
              "Sealed conservative usage lost completed inference or native identity")
        self.report.update(passed=True, sameNativeUUID=True, sameOriginalDeadline=True, finalPhase="cancelled",
            actualExecCommandToolEffect=True, gracefulHeldTurnMove=True, forcedHeldTurnCancellation=True,
            immutableAccountingReplay=True, completedNativeTurnIds=self.completed_ids,
            accounting={"sealedUnits": sealed, "providerCompletedUnits": completed_usage,
                        "conservativeInterruptedInferenceChargeRetained": True,
                        "remainingUnits": cancelled["remaining"]["model.inference"]["units"]["value"]},
            providerRequests=[{key: request[key] for key in ["kind", "turn", "phase", "completed", "usage", "streamError"]
                               if key in request} for request in requests])
        self.save()


def main():
    os.umask(0o077)
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ["output-directory", "home-config", "session-binary", "pty-helper", "provider-directory", "kubeconfig"]:
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--checkpoint-binary", type=Path)
    parser.add_argument("--resume-initial-report", type=Path, help="continue exact retained first local turn after corrected diagnostic")
    parser.add_argument("--resume-first-move-report", type=Path, help="continue exact retained first remote visit after corrected checkpoint diagnostic")
    parser.add_argument("--kubectl", type=Path, default=Path("kubectl"))
    parser.add_argument("--namespace", required=True, help="Registry/AgentRun namespace in dedicated Kind")
    parser.add_argument("--agent-namespace", required=True)
    parser.add_argument("--remote-name", default="remote", help="the remote executor's placement name")
    parser.add_argument("--context", help="kubeconfig context name or file for the remote executor; default: the sproozi context")
    parser.add_argument("--operation-timeout", type=int, default=240)
    parser.add_argument("--turn-timeout", type=int, default=120)
    parser.add_argument("--observe-timeout", type=int, default=30)
    parser.add_argument("--cancel-bound", type=float, default=15)
    parser.add_argument("--profile-server-log", type=Path)
    parser.add_argument("--home-log", type=Path)
    args = parser.parse_args()
    proof = ExtendedProof(args)
    try:
        proof.execute()
    except Exception as error:
        proof.report["failure"] = str(error)
        for name, path in [("profile", args.profile_server_log), ("home", args.home_log)]:
            if path and path.is_file():
                with path.open("rb") as source:
                    source.seek(max(0, path.stat().st_size - (64 << 10)))
                    HOST.write(proof.out / (name + "-failure-tail.log"), source.read(64 << 10).decode(errors="replace"))
        proof.save()
        raise
    finally:
        for child in proof.backgrounds:
            child.close()
    print(json.dumps({"passed": True, "report": str(proof.out / "report.json"), "sessionId": proof.id}))


if __name__ == "__main__":
    main()
