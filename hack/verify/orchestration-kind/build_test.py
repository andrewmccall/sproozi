#!/usr/bin/env python3
"""Offline checks for the Kind image build and import helper."""
import hashlib
import importlib.util
import os
import signal
import stat
import subprocess
import tempfile
import threading
import time
import unittest
from pathlib import Path
from unittest import mock

spec = importlib.util.spec_from_file_location(
    "kind_build", Path(__file__).resolve().parent / "build.py"
)
build = importlib.util.module_from_spec(spec)
spec.loader.exec_module(build)

# A stand-in for docker that models the import paths build.py drives:
#   save TAG              writes a 4 MiB archive whose first line is the tag.
#   exec -i [-e M] NODE … one node's `ctr images import -`: reads stdin in
#                         1 MiB blocks and, on success, stores what it read in
#                         IMPORT_DIR/NODE-TAG.
#   exec NODE sh -c …     the in-node cleanup of marked imports; recorded.
# Faults are counted in FAKE_STATE, so a retry finds the fault used up:
#   SAVE_DIE_TIMES        save writes 1 MiB, then exits 1.
#   STALL_NODE/_TIMES     the import stops reading after 1 MiB and never exits.
#   HANG_NODE/_TIMES      the import reads everything and never exits.
#   FAIL_NODE/_TIMES      the import reads everything, then exits 3.
#   LOCK=1, LOCK_SKEW     containerd's ingest ref lock on a layer two images
#                         share: after its second block an import must hold
#                         the node's lock to read on, and releases it after
#                         its fourth. NODE:TAG pairs in LOCK_SKEW wait 0.5 s
#                         before asking, so each node favours a different
#                         image.
FAKE_DOCKER = r'''#!/usr/bin/env python3
import os, sys, time
state = os.environ["FAKE_STATE"]
open(os.path.join(state, "pids", str(os.getpid())), "w").close()
args = sys.argv[1:]


def take(name):
    path = os.path.join(state, "count-" + name)
    try:
        left = int(open(path).read())
    except FileNotFoundError:
        left = int(os.environ.get(name, "0"))
    if left <= 0:
        return False
    open(path, "w").write(str(left - 1))
    return True


def log(name, line):
    with open(os.path.join(state, name), "a") as stream:
        stream.write(line + "\n")


if args[0] == "save":
    log("saves", args[1])
    data = args[1].encode() + b"\n" + b"".join(bytes([i]) * (1 << 20) for i in range(4)) + b"tail"
    out = sys.stdout.buffer
    if take("SAVE_DIE_TIMES"):
        out.write(data[: 1 << 20])
        out.flush()
        print("docker save: daemon went away", file=sys.stderr)
        sys.exit(1)
    out.write(data)
    sys.exit(0)
if args[0] != "exec":
    sys.exit(64)
rest, interactive = args[1:], False
while rest[0].startswith("-"):
    flag = rest.pop(0)
    if flag == "-i":
        interactive = True
    elif flag == "-e":
        rest.pop(0)
node = rest[0]
if not interactive:
    log("cleanups", node + " " + rest[-1])
    sys.exit(0)
stall = node == os.environ.get("STALL_NODE") and take("STALL_TIMES")
hang = node == os.environ.get("HANG_NODE") and take("HANG_TIMES")
received, reads, lock = bytearray(), 0, None
while block := sys.stdin.buffer.read(1 << 20):
    received += block
    reads += 1
    tag = bytes(received).split(b"\n", 1)[0].decode()
    if stall:
        while True:
            time.sleep(60)
    if reads == 2 and os.environ.get("LOCK"):
        if node + ":" + tag in os.environ.get("LOCK_SKEW", "").split(","):
            time.sleep(0.5)
        path = os.path.join(state, "lock-" + node)
        while True:
            try:
                os.close(os.open(path, os.O_CREAT | os.O_EXCL))
                break
            except FileExistsError:
                time.sleep(0.02)
        lock = path
    if reads == 4 and lock:
        os.unlink(lock)
        lock = None
if lock:
    os.unlink(lock)
while hang:
    time.sleep(60)
if node == os.environ.get("FAIL_NODE") and take("FAIL_TIMES"):
    print("ctr: import failed on " + node)
    sys.exit(3)
tag = bytes(received).split(b"\n", 1)[0].decode()
with open(os.path.join(os.environ["IMPORT_DIR"], node + "-" + tag.replace("/", "_")), "wb") as out:
    out.write(received)
'''


def archive(tag):
    return tag.encode() + b"\n" + b"".join(bytes([i]) * (1 << 20) for i in range(4)) + b"tail"


class ImportArchiveTest(unittest.TestCase):
    """Drive the real import_archive against a fake docker on PATH."""

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        root = Path(self.tmp.name)
        bin_dir = root / "bin"
        bin_dir.mkdir()
        docker = bin_dir / "docker"
        docker.write_text(FAKE_DOCKER)
        docker.chmod(docker.stat().st_mode | stat.S_IXUSR)
        self.imports = root / "imports"
        self.imports.mkdir()
        self.state = root / "state"
        (self.state / "pids").mkdir(parents=True)
        env = {
            "PATH": str(bin_dir) + os.pathsep + os.environ["PATH"],
            "IMPORT_DIR": str(self.imports),
            "FAKE_STATE": str(self.state),
        }
        patchers = [
            mock.patch.dict(os.environ, env),
            # Seconds, not minutes. create=True lets these tests run against
            # a build.py without the bounds, to show it hanging.
            mock.patch.object(build, "IMPORT_STALL_SECONDS", 2, create=True),
            mock.patch.object(build, "IMPORT_EXIT_SECONDS", 3, create=True),
            mock.patch.object(build, "IMPORT_ATTEMPTS", 3, create=True),
        ]
        for patcher in patchers:
            patcher.start()
            self.addCleanup(patcher.stop)
        self.addCleanup(self.kill_leftovers)

    def faults(self, **values):
        patcher = mock.patch.dict(os.environ, {k: str(v) for k, v in values.items()})
        patcher.start()
        self.addCleanup(patcher.stop)

    def alive(self):
        pids = []
        for entry in (self.state / "pids").iterdir():
            try:
                os.kill(int(entry.name), 0)
            except ProcessLookupError:
                continue
            pids.append(int(entry.name))
        return pids

    def kill_leftovers(self):
        for pid in self.alive():
            try:
                os.kill(pid, signal.SIGKILL)
            except ProcessLookupError:
                pass

    def assert_no_child_left(self):
        # The fake docker processes are build.py's own children; one still
        # running, or not yet reaped, after import_archive returns leaked.
        self.assertEqual(self.alive(), [])

    def lines(self, name):
        path = self.state / name
        return path.read_text().splitlines() if path.exists() else []

    def imported(self, node, tag="img:tag"):
        return (self.imports / (node + "-" + tag.replace("/", "_"))).read_bytes()

    def run_bounded(self, calls, timeout=40):
        """Run calls in threads; fail, not hang, if any is still blocked."""
        results = [{} for _ in calls]

        def target(call, result):
            try:
                result["value"] = call()
            except Exception as error:  # noqa: BLE001 - reported by the caller
                result["error"] = error

        threads = [
            threading.Thread(target=target, args=(call, result), daemon=True)
            for call, result in zip(calls, results)
        ]
        for thread in threads:
            thread.start()
        deadline = time.monotonic() + timeout
        for thread in threads:
            thread.join(max(0, deadline - time.monotonic()))
        if any(thread.is_alive() for thread in threads):
            self.kill_leftovers()
            for thread in threads:
                thread.join(10)
            self.fail("import_archive was still blocked after %d s" % timeout)
        return results

    def import_once(self, tag="img:tag"):
        [result] = self.run_bounded(
            [lambda: build.import_archive(tag, ["a", "b"], "linux/arm64")]
        )
        if "error" in result:
            raise result["error"]
        return result["value"]

    def assert_every_node_got(self, checksum, tag="img:tag"):
        expected = archive(tag)
        self.assertEqual(checksum, hashlib.sha256(expected).hexdigest())
        for node in ["a", "b"]:
            self.assertEqual(self.imported(node, tag), expected)

    def test_every_node_receives_the_hashed_archive(self):
        self.assert_every_node_got(self.import_once())
        self.assertEqual(len(self.lines("saves")), 1)
        self.assert_no_child_left()

    def test_a_failed_node_import_fails_the_load_after_every_attempt(self):
        self.faults(FAIL_NODE="b", FAIL_TIMES=99)
        with self.assertRaisesRegex(RuntimeError, "import failed on b"):
            self.import_once()
        self.assertEqual(len(self.lines("saves")), 3)
        self.assert_no_child_left()

    def test_a_node_import_that_fails_once_is_retried(self):
        self.faults(FAIL_NODE="b", FAIL_TIMES=1)
        self.assert_every_node_got(self.import_once())
        self.assertEqual(len(self.lines("saves")), 2)
        self.assert_no_child_left()

    def test_a_node_that_stops_reading_is_abandoned_and_retried(self):
        self.faults(STALL_NODE="b", STALL_TIMES=1)
        started = time.monotonic()
        checksum = self.import_once()
        self.assertLess(time.monotonic() - started, 15)
        self.assert_every_node_got(checksum)
        self.assertEqual(len(self.lines("saves")), 2)
        # The abandoned attempt's imports are also stopped inside both nodes,
        # by that attempt's marker, before the retry starts.
        cleanups = self.lines("cleanups")
        self.assertEqual(sorted(line.split()[0] for line in cleanups), ["a", "b"])
        self.assertEqual(len({line.split()[1] for line in cleanups}), 1)
        self.assert_no_child_left()

    def test_a_node_that_always_stalls_fails_with_what_was_stuck(self):
        self.faults(STALL_NODE="b", STALL_TIMES=99)
        with self.assertRaises(RuntimeError) as raised:
            self.import_once()
        message = str(raised.exception)
        self.assertIn("img:tag", message)
        self.assertIn("after 3 attempts", message)
        self.assertIn("no byte moved for 2 s", message)
        self.assertIn("blocked writing to b", message)
        self.assertRegex(message, r"a accepted \d+ bytes")
        self.assertEqual(len(self.lines("saves")), 3)
        self.assert_no_child_left()

    def test_an_import_that_never_exits_is_abandoned_and_retried(self):
        self.faults(HANG_NODE="a", HANG_TIMES=1)
        self.assert_every_node_got(self.import_once())
        self.assertEqual(len(self.lines("saves")), 2)
        self.assert_no_child_left()

    def test_a_save_that_dies_midway_is_retried(self):
        self.faults(SAVE_DIE_TIMES=1)
        self.assert_every_node_got(self.import_once())
        self.assertEqual(len(self.lines("saves")), 2)
        self.assert_no_child_left()

    def test_a_save_that_always_dies_fails_the_load(self):
        self.faults(SAVE_DIE_TIMES=99)
        with self.assertRaisesRegex(RuntimeError, "docker save img:tag exited 1"):
            self.import_once()
        self.assert_no_child_left()

    def test_two_images_sharing_a_layer_do_not_deadlock_across_nodes(self):
        # A node's containerd lets one import at a time hold a shared layer's
        # ingest ref. Node a favours one image and node b the other: streamed
        # at once, each image's tee blocks on the node where its import waits
        # for the lock the other image holds.
        one, two = "reg/one:t", "reg/two:t"
        self.faults(LOCK=1, LOCK_SKEW="a:" + two + ",b:" + one)
        results = self.run_bounded(
            [
                lambda: build.import_archive(one, ["a", "b"], "linux/arm64"),
                lambda: build.import_archive(two, ["a", "b"], "linux/arm64"),
            ]
        )
        for tag, result in zip([one, two], results):
            self.assertNotIn("error", result)
            self.assert_every_node_got(result["value"], tag)
        # Not rescued by a retry: one save per image.
        self.assertEqual(len(self.lines("saves")), 2)
        self.assert_no_child_left()


class BuildCommandTest(unittest.TestCase):
    def test_local_builds_use_plain_docker_build(self):
        with mock.patch.dict(os.environ, {"SPROOZI_BUILD_CACHE": ""}):
            command = build.build_command("clients", "t:1", "Dockerfile")
        self.assertEqual(command[:2], ["docker", "build"])
        self.assertNotIn("--cache-to", command)

    def test_gha_cache_loads_into_docker_with_a_scope_per_image(self):
        with mock.patch.dict(os.environ, {"SPROOZI_BUILD_CACHE": "gha"}):
            command = build.build_command("clients", "t:1", "Dockerfile")
        self.assertEqual(command[:4], ["docker", "buildx", "build", "--load"])
        self.assertIn("type=gha,scope=clients", command)
        self.assertIn("type=gha,mode=max,scope=clients", command)

    def test_unknown_cache_backend_is_refused(self):
        with mock.patch.dict(os.environ, {"SPROOZI_BUILD_CACHE": "local"}):
            with self.assertRaises(RuntimeError):
                build.build_command("clients", "t:1", "Dockerfile")


PINNED = "docker.io/nousresearch/hermes-agent@sha256:" + "a" * 64


class HermesNodePullTest(unittest.TestCase):
    def test_tag_reference_is_refused_before_any_pull(self):
        with mock.patch.object(build, "kind_nodes") as nodes:
            with self.assertRaises(RuntimeError):
                build.pull_on_nodes("docker.io/nousresearch/hermes-agent:latest")
        nodes.assert_not_called()

    def test_each_node_pulls_and_must_resolve_the_digest(self):
        calls = []

        def run(*args, **kw):
            calls.append(args)
            return subprocess.CompletedProcess(args, 0, "", "")

        def output(*args):
            node = args[2]
            digests = [PINNED] if node == "worker" else ["other@sha256:" + "b" * 64]
            return '{"status": {"repoDigests": %s}}' % str(digests).replace("'", '"')

        with mock.patch.object(build, "kind_nodes", return_value=["worker", "control"]), \
                mock.patch.object(build, "run", side_effect=run), \
                mock.patch.object(build, "output", side_effect=output):
            with self.assertRaises(RuntimeError):
                build.pull_on_nodes(PINNED)
        self.assertEqual(sorted(c[2] for c in calls), ["control", "worker"])
        self.assertTrue(all(c[3:] == ("crictl", "pull", PINNED) for c in calls))

    def test_failed_pull_reports_the_node_and_registry_error(self):
        failed = subprocess.CompletedProcess([], 1, "", "registry: too many requests")
        with mock.patch.object(build, "kind_nodes", return_value=["worker"]), \
                mock.patch.object(build, "run", return_value=failed), \
                mock.patch.object(build, "output") as inspect:
            with self.assertRaisesRegex(
                RuntimeError,
                "Hermes pull failed on worker.*registry: too many requests",
            ):
                build.pull_on_nodes(PINNED)
        inspect.assert_not_called()

    def test_record_for_other_nodes_is_not_reused(self):
        with tempfile.TemporaryDirectory() as tmp:
            out = Path(tmp)
            (out / "hermes.json").write_text(
                '{"image": "%s", "nodes": ["old-control"]}' % PINNED
            )
            with mock.patch.object(build, "OUT", out), \
                    mock.patch.object(build, "kind_nodes", return_value=["new-control"]), \
                    mock.patch.object(build, "pull_on_nodes", return_value={"fresh": True}) as pull:
                self.assertEqual(build.pulled_hermes(PINNED), {"fresh": True})
            pull.assert_called_once_with(PINNED)


if __name__ == "__main__":
    unittest.main()
