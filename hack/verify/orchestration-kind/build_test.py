#!/usr/bin/env python3
"""Offline checks for the Kind image build and import helper."""
import hashlib
import importlib.util
import os
import stat
import tempfile
import unittest
from pathlib import Path
from unittest import mock

spec = importlib.util.spec_from_file_location(
    "kind_build", Path(__file__).resolve().parent / "build.py"
)
build = importlib.util.module_from_spec(spec)
spec.loader.exec_module(build)

# A stand-in for docker: `save` writes a fixed archive larger than one read
# chunk, and `exec -i NODE ctr ... import` stores stdin per node. A node named
# in FAIL_NODE exits non-zero after reading its input.
FAKE_DOCKER = """#!/usr/bin/env python3
import os, sys
args = sys.argv[1:]
if args[0] == "save":
    out = sys.stdout.buffer
    for i in range(3):
        out.write(bytes([i]) * (1 << 20) + b"tail")
    sys.exit(0)
if args[0] == "exec" and args[1] == "-i":
    node = args[2]
    data = sys.stdin.buffer.read()
    with open(os.path.join(os.environ["IMPORT_DIR"], node), "wb") as stream:
        stream.write(data)
    if node == os.environ.get("FAIL_NODE"):
        print("ctr: import failed on " + node)
        sys.exit(3)
    sys.exit(0)
sys.exit(64)
"""


class ImportArchiveTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        root = Path(self.tmp.name)
        bin_dir = root / "bin"
        bin_dir.mkdir()
        docker = bin_dir / "docker"
        docker.write_text(FAKE_DOCKER)
        docker.chmod(docker.stat().st_mode | stat.S_IXUSR)
        self.imports = root / "imports"
        self.imports.mkdir()
        env = {
            "PATH": str(bin_dir) + os.pathsep + os.environ["PATH"],
            "IMPORT_DIR": str(self.imports),
        }
        patcher = mock.patch.dict(os.environ, env)
        patcher.start()
        self.addCleanup(patcher.stop)
        self.addCleanup(self.tmp.cleanup)
        self.expected = b"".join(
            bytes([i]) * (1 << 20) + b"tail" for i in range(3)
        )

    def test_every_node_receives_the_hashed_archive(self):
        checksum = build.import_archive("img:tag", ["a", "b"], "linux/amd64")
        self.assertEqual(checksum, hashlib.sha256(self.expected).hexdigest())
        for node in ["a", "b"]:
            self.assertEqual((self.imports / node).read_bytes(), self.expected)

    def test_a_failed_node_import_fails_the_load(self):
        with mock.patch.dict(os.environ, {"FAIL_NODE": "b"}):
            with self.assertRaisesRegex(RuntimeError, "import failed on b"):
                build.import_archive("img:tag", ["a", "b"], "linux/amd64")


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


if __name__ == "__main__":
    unittest.main()
