"""Boundary regressions for the Kind quickstart driver and owned cleanup."""

import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location("quickstart", Path(__file__).with_name("quickstart.py"))
quickstart = importlib.util.module_from_spec(spec)
spec.loader.exec_module(quickstart)
ROOT = Path(__file__).resolve().parents[2]


def installed_crds():
    return {
        "agentruns.sproozi.com": {"group": "sproozi.com", "kind": "AgentRun"},
        "agentruntimes.sproozi.com": {"group": "sproozi.com", "kind": "AgentRuntime"},
        "agentpolicies.sproozi.com": {"group": "sproozi.com", "kind": "AgentPolicy"},
        "agenttemplates.sproozi.com": {"group": "sproozi.com", "kind": "AgentTemplate"},
        "budgetledgers.sproozi.com": {"group": "sproozi.com", "kind": "BudgetLedger"},
        "networkpolicies.crd.projectcalico.org": {"group": "crd.projectcalico.org", "kind": "NetworkPolicy"},
        "adminnetworkpolicies.policy.networking.k8s.io": {"group": "policy.networking.k8s.io", "kind": "AdminNetworkPolicy"},
        "baselineadminnetworkpolicies.policy.networking.k8s.io": {"group": "policy.networking.k8s.io", "kind": "BaselineAdminNetworkPolicy"},
    }


class Dependencies(unittest.TestCase):
    def test_calico_bundled_admin_policies_are_accepted(self):
        quickstart.check_crds(installed_crds())

    def test_missing_sproozi_crd_fails(self):
        observed = installed_crds()
        del observed["agentruns.sproozi.com"]
        with self.assertRaisesRegex(RuntimeError, "every Sproozi CRD"):
            quickstart.check_crds(observed)

    def test_external_sandbox_cannot_be_a_hidden_dependency(self):
        observed = installed_crds()
        observed["sandboxes.agents.x-k8s.io"] = {"group": "agents.x-k8s.io", "kind": "Sandbox"}
        with self.assertRaisesRegex(RuntimeError, "sandboxes.agents.x-k8s.io"):
            quickstart.check_crds(observed)

    def test_newer_host_kubectl_is_rejected_before_launch(self):
        def output(command):
            if command[0] == "kubectl":
                return json.dumps({"clientVersion": {"major": "1", "minor": "37"}})
            return "available"
        with patch.object(quickstart.shutil, "which", return_value="/tools/bin"), \
                patch.object(quickstart, "capture", side_effect=output):
            with self.assertRaisesRegex(RuntimeError, "pinned Kubernetes 1.33.1"):
                quickstart.doctor()

    def test_kubectl_rejection_names_the_found_version(self):
        def output(command):
            if command[0] == "kubectl":
                return json.dumps({"clientVersion": {"major": "1", "minor": "37", "gitVersion": "v1.37.1"}})
            return "available"
        with patch.object(quickstart.shutil, "which", return_value="/tools/bin"), \
                patch.object(quickstart, "capture", side_effect=output):
            with self.assertRaisesRegex(RuntimeError, r"\(found 1\.37\.1\); run \"make demo-kubectl\""):
                quickstart.doctor()


class GoVersion(unittest.TestCase):
    def minimum(self):
        return (ROOT / "go.mod").read_text().split("\ngo ", 1)[1].split()[0]

    def test_go_older_than_go_mod_is_rejected_with_found_version(self):
        with self.assertRaisesRegex(RuntimeError, "Go " + self.minimum() + r" or later.*found go1\.20\.3"):
            quickstart.check_go("go version go1.20.3 darwin/arm64")

    def test_go_mod_version_and_newer_are_accepted(self):
        quickstart.check_go("go version go" + self.minimum() + " linux/amd64")
        quickstart.check_go("go version go99.0 linux/amd64")


def stub_tools(directory):
    """Host tools that succeed, so check.sh reaches its settings checks without a host."""
    bin_dir = Path(directory) / "bin"
    bin_dir.mkdir()
    for tool in ("docker", "kind", "kubectl", "gh", "go", "make", "openssl"):
        (bin_dir / tool).write_text("#!/bin/sh\nexit 0\n")
        (bin_dir / tool).chmod(0o755)
    return {"PATH": f"{bin_dir}:{os.environ['PATH']}", "HOME": str(directory)}


class DemoEnvironment(unittest.TestCase):
    """doctor --env-file must give hack/demo/check.sh --credentials's verdict without printing values."""

    def doctor(self, contents, mode=0o600):
        with tempfile.TemporaryDirectory() as temporary:
            env_file = Path(temporary) / "demo.env"
            env_file.write_bytes(contents.encode())
            env_file.chmod(mode)
            kubectl = json.dumps({"clientVersion": {"major": "1", "minor": "33"}})
            real_capture = quickstart.capture
            def output(command, **kwargs):
                if command[0] == "bash":
                    return real_capture(command, **kwargs)
                return {"kubectl": kubectl, "go": "go version go99.0 linux/amd64"}.get(command[0], "ok")
            with patch.dict(os.environ, stub_tools(temporary), clear=True), \
                    patch.object(quickstart.shutil, "which", return_value="/tools/bin"), \
                    patch.object(quickstart, "capture", side_effect=output):
                return quickstart.doctor(True, env_file)

    complete = ("GITHUB_APP_ID=1\nGITHUB_APP_INSTALLATION_ID=2\n"
                "GITHUB_APP_PRIVATE_KEY_FILE={key}\nOPENAI_API_KEY=fixture-only\n")

    def with_key(self, contents):
        key = tempfile.NamedTemporaryFile()
        self.addCleanup(key.close)
        return contents.replace("{key}", key.name)

    def test_missing_private_key_file_is_rejected(self):
        with self.assertRaisesRegex(RuntimeError, "GitHub App private key does not exist or is not readable"):
            self.doctor(self.complete.format(key="/nonexistent.pem"))

    def test_readable_private_key_passes(self):
        self.assertEqual(self.doctor(self.with_key(self.complete))["modelAuthMode"], "api_key")

    def test_unknown_model_mode_names_the_value(self):
        with self.assertRaisesRegex(RuntimeError, r"api_key or chatgpt \(found apikey\)"):
            self.doctor("MODEL_AUTH_MODE=apikey\n")

    def test_missing_settings_are_listed_together(self):
        with self.assertRaisesRegex(RuntimeError, "(?s)Set these 4 demo settings in .*OPENAI_API_KEY.*"
                                    "GITHUB_APP_ID.*GITHUB_APP_INSTALLATION_ID.*GITHUB_APP_PRIVATE_KEY_FILE"):
            self.doctor("")

    def test_env_file_that_runs_commands_is_rejected_without_running_or_echoing_it(self):
        with tempfile.TemporaryDirectory() as temporary:
            marker = Path(temporary) / "ran"
            for line in (f"touch {marker}", f"OPENAI_API_KEY=sk touch {marker}",
                         f"OPENAI_API_KEY=$(touch {marker})", f"OPENAI_API_KEY=`touch {marker}`"):
                with self.subTest(line=line):
                    with self.assertRaisesRegex(RuntimeError, "line 1 is not a plain KEY=value assignment") as raised:
                        self.doctor(line + "\n")
                    self.assertNotIn("touch", str(raised.exception))
                    self.assertFalse(marker.exists())

    def test_loading_the_file_never_runs_commands_even_past_check_sh(self):
        """Every read goes through demo_env_load, so a file check.sh never saw is still refused."""
        with tempfile.TemporaryDirectory() as temporary:
            marker = Path(temporary) / "MARKER"
            env_file = Path(temporary) / "demo.env"
            for line in (f"SPROOZI_CNI_MANIFEST=$(touch {marker})", f"BASH_ENV={marker}", f"touch {marker}"):
                with self.subTest(line=line):
                    env_file.write_text(line + "\n")
                    for checkout in (ROOT, Path(temporary) / "checkout"):
                        if checkout != ROOT and not checkout.exists():
                            (checkout / "hack/demo").mkdir(parents=True)
                            (checkout / "hack/demo/env.sh").write_text((ROOT / "hack/demo/env.sh").read_text())
                        with self.assertRaisesRegex(RuntimeError, "^bash exited 1: .*demo.env line 1 ") as raised:
                            quickstart.read_demo_env(env_file, "printf %s \"$SPROOZI_CNI_MANIFEST\"", checkout)
                        self.assertNotIn("touch", str(raised.exception))
                        self.assertFalse(marker.exists())
            passed = subprocess.CompletedProcess([], 0, "", "")
            env_file.write_text(f"OPENAI_API_KEY=$(touch {marker})\n")
            with patch.object(quickstart.subprocess, "run", side_effect=lambda command, **kwargs: passed
                              if "check.sh" in " ".join(command) else Cleanup.real_run(command, **kwargs)):
                with self.assertRaisesRegex(RuntimeError, "line 1 is not a plain KEY=value assignment"):
                    quickstart.check_demo_settings(env_file)
            self.assertFalse(marker.exists())

    def test_unterminated_quote_is_rejected(self):
        with self.assertRaisesRegex(RuntimeError, "line 5 is not a plain KEY=value assignment"):
            self.doctor(self.with_key(self.complete) + 'SPROOZI_DEMO_REPOSITORY="owner/name\n')

    def test_zero_app_id_is_rejected_as_check_sh_does(self):
        with self.assertRaisesRegex(RuntimeError, "GITHUB_APP_ID must be a positive integer"):
            self.doctor(self.with_key(self.complete).replace("GITHUB_APP_ID=1", "GITHUB_APP_ID=0"))

    def test_quoted_values_from_configure_are_accepted(self):
        contents = self.with_key(self.complete) + "SPROOZI_DEMO_REPOSITORY=owner/name\n" \
            "CODEX_IMAGE=a\\ b$'\\n'c # written by printf %q\nexport MODEL_AUTH_MODE='api_key'\n"
        self.assertEqual(self.doctor(contents)["modelAuthMode"], "api_key")

    def test_unreadable_env_file_says_cannot_read(self):
        if os.geteuid() == 0:
            self.skipTest("root reads every file")
        with self.assertRaisesRegex(RuntimeError, "^cannot read .*demo.env$"):
            self.doctor(self.complete, mode=0)

    def test_crlf_env_file_is_rejected_with_line_ending_advice(self):
        with self.assertRaisesRegex(RuntimeError, r"CRLF\) line endings"):
            self.doctor(self.with_key(self.complete).replace("\n", "\r\n"))


class DemoCheck(unittest.TestCase):
    def test_every_missing_setting_is_reported_in_one_run(self):
        with tempfile.TemporaryDirectory() as temporary:
            env_file = Path(temporary) / "demo.env"
            env_file.write_text("")
            env = {"PATH": os.environ["PATH"], "HOME": temporary, "SPROOZI_DEMO_ENV_FILE": str(env_file)}
            check = subprocess.run(["bash", str(ROOT / "hack/demo/check.sh")], env=env,
                                   capture_output=True, text=True, timeout=30)
        self.assertEqual(check.returncode, 1)
        for name in ("OPENAI_API_KEY", "GITHUB_APP_ID", "GITHUB_APP_INSTALLATION_ID",
                     "GITHUB_APP_PRIVATE_KEY_FILE", "SPROOZI_CNI_MANIFEST", "SPROOZI_CNI_SELECTOR",
                     "SPROOZI_DEMO_REPOSITORY", "CODEX_IMAGE"):
            self.assertIn(name, check.stderr)
        self.assertIn("Set these 8 demo settings in " + str(env_file), check.stderr)


RUN_ID = "20261007t010203z-01234567"


def evidence(root, run_id=RUN_ID, **owner):
    """An evidence directory whose owner.json records owner (a demo run by default)."""
    proof = root / run_id
    proof.mkdir()
    state = {"path": "demo", "cluster": "sproozi-quickstart-" + run_id, **owner}
    (proof / "owner.json").write_text(json.dumps(state))
    return proof


def live_process(test):
    """A sleep process standing in for a quickstart run that is still going."""
    process = subprocess.Popen(["sleep", "60"])
    test.addCleanup(process.wait)
    test.addCleanup(process.kill)
    return process


class Owner(unittest.TestCase):
    def test_live_process_with_matching_start_is_running(self):
        process = live_process(self)
        state = {"pid": process.pid, "started": quickstart.process_start(process.pid)}
        self.assertTrue(state["started"])
        self.assertEqual(quickstart.owner_process(state), "running")

    def test_reused_pid_with_another_start_time_is_stopped(self):
        process = live_process(self)
        state = {"pid": process.pid, "started": "Thu Jan  1 00:00:00 1970"}
        self.assertEqual(quickstart.owner_process(state), "stopped")

    def test_exited_process_is_stopped(self):
        process = subprocess.Popen(["true"])
        process.wait()
        self.assertEqual(quickstart.owner_process({"pid": process.pid, "started": "x"}), "stopped")

    def test_missing_pid_is_unknown(self):
        self.assertEqual(quickstart.owner_process({}), "unknown")
        self.assertEqual(quickstart.owner_process({"pid": True}), "unknown")


class CommandLog(unittest.TestCase):
    def test_gates_write_plain_logs_without_changing_the_caller_environment(self):
        with tempfile.TemporaryDirectory() as directory:
            log = Path(directory) / "commands.log"
            env = dict(os.environ, GINKGO_NO_COLOR="", NO_COLOR="")
            quickstart.run_command(["sh", "-c", 'printf "%s %s" "$GINKGO_NO_COLOR" "$NO_COLOR"'],
                                   directory, env, log)
            self.assertTrue(log.read_text().endswith("TRUE 1"))
            self.assertEqual(env["NO_COLOR"], "")


class DemoCheckLineEndings(unittest.TestCase):
    def test_crlf_file_is_rejected_before_a_key_path_is_misreported(self):
        with tempfile.TemporaryDirectory() as temporary, tempfile.NamedTemporaryFile() as key:
            env_file = Path(temporary) / "demo.env"
            env_file.write_bytes(f"GITHUB_APP_PRIVATE_KEY_FILE={key.name}\r\n".encode())
            env = dict(stub_tools(temporary), SPROOZI_DEMO_ENV_FILE=str(env_file))
            check = subprocess.run(["bash", str(ROOT / "hack/demo/check.sh"), "--credentials"], env=env,
                                   capture_output=True, text=True, timeout=30)
        self.assertEqual(check.returncode, 1)
        self.assertIn(f"{env_file} has Windows (CRLF) line endings", check.stderr)
        self.assertNotIn("does not exist", check.stderr)


class Lock(unittest.TestCase):
    def held(self, **owner):
        """verify() against a driver.lock held by RUN_ID, returning the error and the evidence names."""
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            proof = evidence(root, **owner)
            lock = root / "driver.lock"
            lock.mkdir()
            (lock / "owner").write_text(proof.name + "\n")
            args = quickstart.argparse.Namespace(path="dependencies", env_file=None, keep_cluster=False)
            with patch.object(quickstart, "EVIDENCE_ROOT", root), patch.object(quickstart, "LOCK", lock), \
                    patch.object(quickstart, "doctor", return_value={}), \
                    patch.object(quickstart, "capture", return_value=""), \
                    patch.object(quickstart, "say"):
                with self.assertRaises(RuntimeError) as raised:
                    quickstart.verify(args)
            self.assertEqual({path.name for path in root.iterdir()}, {"driver.lock", proof.name})
            return str(raised.exception), str(proof)

    def test_live_owner_is_named_and_no_cleanup_command_is_offered(self):
        process = live_process(self)
        message, proof = self.held(pid=process.pid, started=quickstart.process_start(process.pid))
        self.assertRegex(message, f"(?s)Dependencies check failed: .*still running \\(PID {process.pid}, "
                         f"started .*\\).*{proof}")
        self.assertNotIn("cleanup", message)

    def test_stopped_owner_gets_the_cleanup_command(self):
        process = subprocess.Popen(["true"])
        process.wait()
        message, proof = self.held(pid=process.pid, started="x")
        self.assertRegex(message, f"(?s)process has exited.*quickstart\\.py cleanup {proof}$")
        self.assertNotIn("--force", message)

    def test_owner_without_pid_needs_force(self):
        message, proof = self.held()
        self.assertRegex(message, f"(?s)cannot tell whether it is still running.*cleanup --force {proof}$")

    def test_live_owner_without_start_time_needs_force(self):
        message, proof = self.held(pid=os.getpid(), started=None)
        self.assertRegex(message, f"(?s)records no start time.*cleanup --force {proof}$")


class Interrupt(unittest.TestCase):
    def test_owner_json_records_this_process(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            args = quickstart.argparse.Namespace(path="dependencies", env_file=None, keep_cluster=False)
            seen = {}
            def record(destination):
                seen.update(json.loads((destination.parent / "owner.json").read_text()))
                raise quickstart.Interrupted("SIGINT")
            with patch.object(quickstart, "EVIDENCE_ROOT", root), \
                    patch.object(quickstart, "LOCK", root / "driver.lock"), \
                    patch.object(quickstart, "doctor", return_value={}), \
                    patch.object(quickstart, "capture", return_value=""), \
                    patch.object(quickstart, "snapshot", side_effect=record), \
                    patch.object(quickstart, "hold_signals"), \
                    patch.object(quickstart, "say"):
                quickstart.verify(args)
            self.assertEqual(seen["pid"], os.getpid())
            self.assertEqual(seen["started"], quickstart.process_start(os.getpid()))
            self.assertTrue(seen["cluster"].startswith(quickstart.DEPENDENCY_CLUSTER_PREFIX))

    def test_signal_is_recorded_and_owned_run_is_cleaned_up(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            args = quickstart.argparse.Namespace(path="dependencies", env_file=None, keep_cluster=False)
            def hangup(_):
                raise quickstart.Interrupted("SIGHUP")
            with patch.object(quickstart, "EVIDENCE_ROOT", root), \
                    patch.object(quickstart, "LOCK", root / "driver.lock"), \
                    patch.object(quickstart, "doctor", return_value={}), \
                    patch.object(quickstart, "capture", return_value=""), \
                    patch.object(quickstart, "snapshot", side_effect=hangup), \
                    patch.object(quickstart, "hold_signals"), \
                    patch.object(quickstart, "say"):
                self.assertEqual(quickstart.verify(args), 1)
            [run] = [path for path in root.iterdir()]
            result = json.loads((run / "result.json").read_text())
            self.assertEqual(result["error"], "Interrupted by SIGHUP")
            self.assertEqual(result["interrupted"], "SIGHUP")
            self.assertFalse((root / "driver.lock").exists())


class Cleanup(unittest.TestCase):
    def test_foreign_directory_never_reaches_cluster_deletion(self):
        with tempfile.TemporaryDirectory() as temporary, \
                patch.object(quickstart, "EVIDENCE_ROOT", Path(temporary) / "proof"), \
                patch.object(quickstart.subprocess, "run") as run:
            with self.assertRaisesRegex(RuntimeError, "evidence directory"):
                quickstart.cleanup(Path(temporary))
            run.assert_not_called()

    def test_mismatched_cluster_owner_is_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            proof = root / "20261007t010203z-01234567"
            proof.mkdir()
            (proof / "owner.json").write_text(json.dumps({"path": "demo", "cluster": "real-dev-cluster"}))
            with patch.object(quickstart, "EVIDENCE_ROOT", root), \
                    patch.object(quickstart.subprocess, "run") as run:
                with self.assertRaisesRegex(RuntimeError, "recorded verification owner"):
                    quickstart.cleanup(proof)
                run.assert_not_called()

    def test_owned_cleanup_preserves_proof_and_other_cluster(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            run_id = "20261007t010203z-01234567"
            cluster = "sproozi-quickstart-" + run_id
            proof = root / run_id
            work = proof / "checkout"
            work.mkdir(parents=True)
            (work / "private-input").write_text("scratch")
            (proof / "commands.log").write_text("proof")
            (proof / "owner.json").write_text(json.dumps({"path": "demo", "cluster": cluster}))
            (proof / "result.json").write_text(json.dumps({"outcome": "passed"}))
            lock = root / "driver.lock"
            lock.mkdir()
            (lock / "owner").write_text(run_id)
            with patch.object(quickstart, "EVIDENCE_ROOT", root), \
                    patch.object(quickstart, "LOCK", lock), \
                    patch.object(quickstart, "capture", side_effect=["real-dev-cluster\n" + cluster, "real-dev-cluster"]), \
                    patch.object(quickstart.subprocess, "run") as run:
                quickstart.cleanup(proof, force=True)
                run.assert_called_once_with(["kind", "delete", "cluster", "--name", cluster], check=True,
                                            timeout=120, capture_output=True)
            self.assertFalse(work.exists())
            self.assertFalse(lock.exists())
            self.assertEqual((proof / "commands.log").read_text(), "proof")
            result = json.loads((proof / "result.json").read_text())
            self.assertTrue(result["clusterRemoved"])
            self.assertTrue(result["scratchRemoved"])

    real_run = staticmethod(subprocess.run)

    def only_ps(self, command, **kwargs):
        """Let the owner check read ps; record every other command without running it."""
        return self.real_run(command, **kwargs) if command[0] == "ps" else None

    def cleanup_error(self, prepare, lock_owner=None, **kwargs):
        """cleanup's one-line error for a directory that prepare(root) returns, without touching Kind."""
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            target = prepare(root)
            lock = root / "driver.lock"
            if lock_owner is not None:
                lock.mkdir()
                if lock_owner:
                    (lock / "owner").write_text(lock_owner)
            with patch.object(quickstart, "EVIDENCE_ROOT", root), patch.object(quickstart, "LOCK", lock), \
                    patch.object(quickstart, "capture", return_value=""), \
                    patch.object(quickstart, "say") as said, \
                    patch.object(quickstart.subprocess, "run", side_effect=self.only_ps) as run:
                try:
                    quickstart.cleanup(target, **kwargs)
                except RuntimeError as error:
                    self.assertFalse([call for call in run.call_args_list if call.args[0][0] == "kind"])
                    self.assertNotIn("\n", str(error))
                    return str(error)
                return [call.args[0] for call in said.call_args_list]

    def test_live_owner_is_refused_and_named(self):
        process = live_process(self)
        started = quickstart.process_start(process.pid)
        message = self.cleanup_error(lambda root: evidence(root, pid=process.pid, started=started))
        self.assertRegex(message, f"still running \\(PID {process.pid}, started .*\\).*--force")

    def test_force_cleans_up_a_live_owner(self):
        process = live_process(self)
        started = quickstart.process_start(process.pid)
        said = self.cleanup_error(lambda root: evidence(root, pid=process.pid, started=started), force=True)
        self.assertIn("Kept evidence in", said[-1])

    def test_stopped_owner_is_cleaned_without_force_and_absent_cluster_reads_as_done(self):
        process = subprocess.Popen(["true"])
        process.wait()
        said = self.cleanup_error(lambda root: evidence(root, pid=process.pid, started="x"))
        self.assertIn("Kind cluster sproozi-quickstart-" + RUN_ID + " was already removed", said)

    def test_owning_run_confirms_a_cluster_the_gate_removed(self):
        said = self.cleanup_error(lambda root: evidence(root, pid=os.getpid()), from_run=True)
        self.assertIn("Kind cluster sproozi-quickstart-" + RUN_ID + " removed", said)

    def test_live_owner_without_start_time_is_refused(self):
        for started in ({}, {"started": None}, {"started": ""}):
            with self.subTest(started=started):
                message = self.cleanup_error(lambda root: evidence(root, pid=os.getpid(), **started))
                self.assertRegex(message, f"PID {os.getpid()} is running, but its owner.json records no start "
                                 f"time.*add --force")

    def test_live_owner_without_start_time_is_cleaned_with_force(self):
        said = self.cleanup_error(lambda root: evidence(root, pid=os.getpid(), started=None), force=True)
        self.assertIn("Kept evidence in", said[-1])

    def test_symlinked_checkout_is_left_alone_with_a_clear_message(self):
        with tempfile.TemporaryDirectory() as outside:
            (Path(outside) / "keep").write_text("not ours")
            def prepare(root):
                proof = evidence(root, pid=os.getpid())
                (proof / "checkout").symlink_to(outside)
                return proof
            message = self.cleanup_error(prepare, lock_owner=RUN_ID, from_run=True)
            self.assertRegex(message, "checkout is a symbolic link, not a scratch checkout.*rm .*checkout$")
            self.assertEqual((Path(outside) / "keep").read_text(), "not ours")

    def test_result_that_is_not_an_object_is_one_line(self):
        for contents in ("[]", "{not json", "3"):
            with self.subTest(contents=contents):
                def prepare(root):
                    proof = evidence(root, pid=os.getpid())
                    (proof / "result.json").write_text(contents)
                    return proof
                message = self.cleanup_error(prepare, from_run=True)
                self.assertRegex(message, "result.json is not a quickstart result \\(a JSON object\\); the "
                                 "cluster and checkout are removed$")

    def test_absent_directory_says_it_does_not_exist(self):
        self.assertRegex(self.cleanup_error(lambda root: root / RUN_ID), RUN_ID + " does not exist$")

    def test_malformed_and_keyless_owner_records_give_one_line(self):
        for contents in ("{not json", "{}", "[]"):
            with self.subTest(contents=contents):
                def prepare(root):
                    (root / RUN_ID).mkdir()
                    (root / RUN_ID / "owner.json").write_text(contents)
                    return root / RUN_ID
                self.assertRegex(self.cleanup_error(prepare), "owner.json is not a readable quickstart owner record")

    def test_lock_without_owner_file_is_left_alone(self):
        said = self.cleanup_error(lambda root: evidence(root, pid=os.getpid()), lock_owner="", from_run=True)
        self.assertIn("Kept evidence in", said[-1])

    def test_legacy_dependency_owner_still_matches_the_kind_sh_cluster_name(self):
        said = self.cleanup_error(lambda root: evidence(
            root, path="dependencies", cluster=quickstart.DEPENDENCY_CLUSTER_PREFIX + RUN_ID), force=True)
        self.assertIn("Kept evidence in", said[-1])


def dead_pid():
    process = subprocess.Popen(["true"])
    process.wait()
    return process.pid


def gate_group(test, script="sleep 60 & sleep 60"):
    """A new process group standing in for a crashed run's make test-e2e; returns its group ID."""
    leader = subprocess.Popen(["sh", "-c", script], start_new_session=True)
    def stop():
        try:
            os.killpg(leader.pid, quickstart.signal.SIGKILL)
        except (ProcessLookupError, PermissionError):
            pass  # Gone, or only zombies remain (macOS refuses to signal those).
        leader.wait()
    test.addCleanup(stop)
    for _ in range(50):
        if len(quickstart.group_processes(leader.pid) or {}) >= 2:
            break
        quickstart.time.sleep(0.05)
    return leader


class CrashedRun(unittest.TestCase):
    """cleanup after kill -9 stops the run's gate process group, and only that group."""

    real_run = staticmethod(subprocess.run)
    only_ps, cleanup_error = Cleanup.only_ps, Cleanup.cleanup_error

    def test_dead_owner_group_is_stopped_before_the_cluster_is_deleted(self):
        leader = gate_group(self)
        group = {"pgid": leader.pid, "started": quickstart.process_start(leader.pid)}
        said = self.cleanup_error(lambda root: evidence(root, pid=dead_pid(), started="x", group=group))
        self.assertIsInstance(said, list, said)
        self.assertEqual(leader.wait(timeout=15), -quickstart.signal.SIGTERM)
        self.assertEqual(quickstart.group_processes(leader.pid), {})
        self.assertRegex(said[0], f"^Stopping process group {leader.pid} left by the stopped run \\(PIDs ")
        self.assertEqual(said[1], f"Stopped process group {leader.pid} (SIGTERM)")
        self.assertIn("was already removed", said[2])

    def test_group_that_ignores_term_is_killed(self):
        leader = gate_group(self, "trap '' TERM; sleep 60 & wait")
        group = {"pgid": leader.pid, "started": quickstart.process_start(leader.pid)}
        with patch.object(quickstart, "wait_for_group",
                          side_effect=lambda pgid, _: quickstart.time.sleep(0.5) or quickstart.group_processes(pgid)):
            said = self.cleanup_error(lambda root: evidence(root, pid=dead_pid(), started="x", group=group))
        self.assertIn(f"Stopped process group {leader.pid} (SIGKILL)", said)

    def test_group_id_reused_by_another_process_is_never_signalled(self):
        leader = gate_group(self)
        group = {"pgid": leader.pid, "started": "Thu Jan  1 00:00:00 1970"}
        with patch.object(quickstart.os, "killpg") as killpg:
            said = self.cleanup_error(lambda root: evidence(root, pid=dead_pid(), started="x", group=group))
        killpg.assert_not_called()
        self.assertIsNone(leader.poll())
        self.assertFalse([line for line in said if "process group" in line])

    def test_group_without_start_time_is_refused_without_force(self):
        leader = gate_group(self)
        group = {"pgid": leader.pid, "started": None}
        with patch.object(quickstart.os, "killpg") as killpg:
            message = self.cleanup_error(lambda root: evidence(root, pid=dead_pid(), started="x", group=group))
            self.assertRegex(message, f"process group {leader.pid} may still be running, but owner.json records "
                             "no start time for it.*--force")
            said = self.cleanup_error(lambda root: evidence(root, pid=dead_pid(), started="x", group=group),
                                      force=True)
        killpg.assert_not_called()
        self.assertIn(f"Did not stop process group {leader.pid}", said[0])

    def test_group_whose_leader_exited_is_not_signalled(self):
        leader = gate_group(self, "sleep 60 & sleep 60 & exit 0")
        leader.wait()
        members = quickstart.group_processes(leader.pid)
        self.assertTrue(members)
        group = {"pgid": leader.pid, "started": "x"}
        with patch.object(quickstart.os, "killpg") as killpg:
            message = self.cleanup_error(lambda root: evidence(root, pid=dead_pid(), started="x", group=group))
        killpg.assert_not_called()
        self.assertRegex(message, "its leader has exited and it cannot confirm that PIDs")

    def test_live_owner_group_is_left_to_the_owner_even_with_force(self):
        leader = gate_group(self)
        group = {"pgid": leader.pid, "started": quickstart.process_start(leader.pid)}
        with patch.object(quickstart.os, "killpg") as killpg:
            self.cleanup_error(lambda root: evidence(root, pid=os.getpid(),
                                                     started=quickstart.process_start(os.getpid()), group=group),
                               force=True)
        killpg.assert_not_called()

    def test_verify_records_each_gate_group_in_owner_json(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            args = quickstart.argparse.Namespace(path="dependencies", env_file=None, keep_cluster=False)
            seen = []
            def gate(command, work, env, log, started_group):
                started_group(quickstart.argparse.Namespace(pid=os.getpid()))
                seen.append(json.loads((work.parent / "owner.json").read_text()))
                raise RuntimeError("stop here")
            with patch.object(quickstart, "EVIDENCE_ROOT", root), \
                    patch.object(quickstart, "LOCK", root / "driver.lock"), \
                    patch.object(quickstart, "doctor", return_value={}), \
                    patch.object(quickstart, "capture", return_value=""), \
                    patch.object(quickstart, "snapshot", side_effect=lambda work: work.mkdir() or {}), \
                    patch.object(quickstart, "run_command", side_effect=gate), \
                    patch.object(quickstart, "hold_signals"), \
                    patch.object(quickstart, "say"):
                quickstart.verify(args)
        self.assertEqual(seen[0]["group"], {"pgid": os.getpid(), "started": quickstart.process_start(os.getpid())})
        self.assertEqual(seen[0]["pid"], os.getpid())


class Failure(unittest.TestCase):
    def test_failed_gate_prints_its_log_once_with_the_last_error_lines(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            args = quickstart.argparse.Namespace(path="dependencies", env_file=None, keep_cluster=False)
            def gate(command, work, env, log, *_):
                log.write_text("\n$ make test-e2e\nbuilding\n\x1b[31mERROR: image build failed: no space left\x1b[0m\n"
                               "make: *** [test-e2e] Error 2\n")
                raise RuntimeError("make test-e2e exited 2")
            with patch.object(quickstart, "EVIDENCE_ROOT", root), \
                    patch.object(quickstart, "LOCK", root / "driver.lock"), \
                    patch.object(quickstart, "doctor", return_value={}), \
                    patch.object(quickstart, "capture", return_value=""), \
                    patch.object(quickstart, "snapshot", side_effect=lambda work: work.mkdir() or {}), \
                    patch.object(quickstart, "run_command", side_effect=gate), \
                    patch.object(quickstart, "observe_crds"), \
                    patch.object(quickstart, "hold_signals"), \
                    patch.object(quickstart, "say") as said:
                self.assertEqual(quickstart.verify(args), 1)
            output = "\n".join(call.args[0] for call in said.call_args_list)
        tail = output[output.index("Dependencies check failed"):]
        self.assertIn("Dependencies check failed after 0s: make test-e2e exited 2\n", tail)
        self.assertEqual(tail.count("commands.log"), 1)
        self.assertIn("    ERROR: image build failed: no space left", tail)
        self.assertNotIn("make: ***", tail)
        self.assertNotIn("see ", tail)

    def test_capture_failure_is_one_line_with_the_last_error(self):
        with self.assertRaisesRegex(RuntimeError, r"^bash exited 3: second$"):
            quickstart.capture(["bash", "-c", "echo first >&2; echo second >&2; exit 3"])


class Doctor(unittest.TestCase):
    def run_main(self, *arguments, go="go version go99.0.1 darwin/arm64", status=0):
        kubectl = json.dumps({"clientVersion": {"major": "1", "minor": "33", "gitVersion": "v1.33.1"}})
        outputs = {"docker": "28.0", "kind": "kind v0.29.0", "kubectl": kubectl, "go": go}
        printed = []
        with patch.object(quickstart.shutil, "which", return_value="/tools/bin"), \
                patch.object(quickstart, "capture", side_effect=lambda command: outputs[command[0]]), \
                patch("sys.argv", ["quickstart.py", "doctor", *arguments]), \
                patch("builtins.print", side_effect=lambda *a, **_: printed.append(" ".join(map(str, a)))):
            self.assertEqual(quickstart.main(), status)
        return "\n".join(printed)

    def test_human_output_is_checks_and_a_verdict(self):
        output = self.run_main()
        self.assertNotIn("{", output)
        self.assertIn("  ok  kubectl 1.33.1", output)
        self.assertTrue(output.endswith("Doctor checks passed: host tools and versions"))

    def test_go_line_names_the_go_mod_minimum(self):
        minimum = GoVersion().minimum()
        self.assertIn(f"  ok  go version go99.0.1 darwin/arm64 (go.mod requires {minimum})", self.run_main())

    def test_failure_lists_passed_checks_before_the_error(self):
        lines = []
        kubectl = json.dumps({"clientVersion": {"major": "1", "minor": "33", "gitVersion": "v1.33.1"}})
        outputs = {"docker": "28.0", "kind": "kind v0.29.0", "kubectl": kubectl, "go": "go version go1.20.3 x"}
        with patch.object(quickstart.shutil, "which", return_value="/tools/bin"), \
                patch.object(quickstart, "capture", side_effect=lambda command: outputs[command[0]]), \
                patch("sys.argv", ["quickstart.py", "doctor"]), \
                patch("builtins.print", side_effect=lambda *a, **_: lines.append(" ".join(map(str, a)))):
            with self.assertRaisesRegex(RuntimeError, "^Doctor check failed: Use Go .* or later"):
                quickstart.main()
        self.assertEqual(lines, ["  ok  tools on PATH: " + ", ".join(quickstart.TOOLS), "  ok  Docker server 28.0",
                                 "  ok  kind v0.29.0", "  ok  kubectl 1.33.1"])

    def test_every_failure_is_listed_together(self):
        lines = []
        kubectl = json.dumps({"clientVersion": {"major": "1", "minor": "37", "gitVersion": "v1.37.1"}})

        def output(command):
            if command[0] == "docker":
                raise RuntimeError("docker info exited 1: Cannot connect to the Docker daemon")
            return {"kind": "kind v0.29.0", "kubectl": kubectl, "go": "go version go1.20.3 x"}[command[0]]
        with patch.object(quickstart.shutil, "which", return_value="/tools/bin"), \
                patch.object(quickstart, "capture", side_effect=output), \
                patch("sys.argv", ["quickstart.py", "doctor"]), \
                patch("builtins.print", side_effect=lambda *a, **_: lines.append(" ".join(map(str, a)))):
            with self.assertRaisesRegex(RuntimeError, r"^Doctor check failed:\n  docker info exited 1: Cannot connect "
                                        r"to the Docker daemon\n  Use kubectl 1\.32, 1\.33 or 1\.34 .*\(found 1\.37\.1\)"
                                        r".*\n  Use Go .* or later, as go.mod requires \(found go1\.20\.3\)$"):
                quickstart.main()
        self.assertEqual(lines, ["  ok  tools on PATH: " + ", ".join(quickstart.TOOLS), "  ok  kind v0.29.0"])

    def test_missing_tool_skips_only_its_own_check(self):
        lines = []
        kubectl = json.dumps({"clientVersion": {"major": "1", "minor": "33", "gitVersion": "v1.33.1"}})
        outputs = {"docker": "28.0", "kubectl": kubectl, "go": "go version go99.0 x"}
        with patch.object(quickstart.shutil, "which", side_effect=lambda tool: None if tool == "kind" else "/bin"), \
                patch.object(quickstart, "capture", side_effect=lambda command: outputs[command[0]]):
            with self.assertRaisesRegex(RuntimeError, "^Missing tools: kind$"):
                quickstart.doctor(passed=lines.append)
        self.assertEqual(lines[0], "Docker server 28.0")
        self.assertEqual(lines[1], "kubectl 1.33.1")

    def test_json_flag_prints_only_json(self):
        report = json.loads(self.run_main("--json"))
        self.assertTrue(report["ok"])
        self.assertEqual(report["dockerServer"], "28.0")

    def test_json_failure_is_an_object_and_exits_1(self):
        report = json.loads(self.run_main("--json", go="go version go1.20.3 x", status=1))
        self.assertFalse(report["ok"])
        self.assertRegex(report["error"], "^Use Go .* or later")
        self.assertEqual(report["passed"][-1], "kubectl 1.33.1")

    def test_evidence_without_reachable_cluster_is_one_clean_line(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            proof = evidence(root)
            (proof / "cluster.kubeconfig").write_text("")
            def output(command, **_):
                if "current-context" in command:
                    return "kind-sproozi-quickstart-" + RUN_ID
                raise RuntimeError("kubectl --kubeconfig exited 1: connection refused")
            with patch.object(quickstart, "EVIDENCE_ROOT", root), \
                    patch.object(quickstart, "capture", side_effect=output):
                with self.assertRaisesRegex(RuntimeError, "^Kind cluster sproozi-quickstart-" + RUN_ID
                                            + " is not reachable: kubectl --kubeconfig exited 1: connection refused$"):
                    quickstart.check_evidence(proof)


if __name__ == "__main__":
    unittest.main()
