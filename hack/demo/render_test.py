"""Input regressions for the demo renderer: placeholders never reach a cluster."""

import importlib.util
from pathlib import Path
import subprocess
import sys
import unittest


spec = importlib.util.spec_from_file_location("render", Path(__file__).with_name("render.py"))
render = importlib.util.module_from_spec(spec)
spec.loader.exec_module(render)
IMAGE = "example/codex@sha256:" + "0" * 64


class RepositoryTest(unittest.TestCase):
    def test_accepts_real_looking_repositories(self):
        for value in ("octo-org/sproozi-demo", "a/b", "Octo1/demo_repo.v2"):
            self.assertIsNone(render.repository_error(value), value)

    def test_rejects_placeholders(self):
        for value in ("YOUR-ACCOUNT/sproozi-demo", "your_org/demo", "owner/name", "OWNER/demo",
                      "example/sproozi-demo", "octo-org/repo"):
            error = render.repository_error(value)
            self.assertIsNotNone(error, value)
            self.assertIn("placeholder", error)

    def test_rejects_malformed_values(self):
        for value in ("sproozi-demo", "a/b/c", "-octo/demo", "octo-/demo", "octo_org/demo", "octo/..",
                      "octo/", " octo/demo", "https://github.com/octo/demo"):
            error = render.repository_error(value)
            self.assertIsNotNone(error, value)
            self.assertIn("owner/name", error)

    def test_cli_exits_before_rendering(self):
        result = subprocess.run(
            [sys.executable, str(Path(__file__).with_name("render.py")), "--codex-image", IMAGE,
             "--repository", "YOUR-ACCOUNT/sproozi-demo"],
            capture_output=True, text=True, check=False,
        )
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("placeholder", result.stderr)
        self.assertEqual(result.stdout, "")


if __name__ == "__main__":
    unittest.main()
