"""Tests for the release notes publish.py writes."""

import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("publish", Path(__file__).resolve().parent / "publish.py")
publish = importlib.util.module_from_spec(spec)
spec.loader.exec_module(publish)


class BreakingChangeNotes(unittest.TestCase):
    def test_footer_instructions_reach_the_notes(self):
        body = ("Move the ledger.\n\nBREAKING CHANGE: delete the old Role before upgrading:\n"
                "kubectl delete role old -n sproozi-system\n\nCo-Authored-By: Someone <a@b>\n")
        footer = publish.breaking_footer(body)
        self.assertIn("kubectl delete role old -n sproozi-system", footer)
        self.assertNotIn("Co-Authored-By", footer)
        notes = publish.notes("v0.2.0", [("refactor: move the ledger", footer)])
        self.assertIn("refactor: move the ledger", notes)
        self.assertIn("kubectl delete role old -n sproozi-system", notes)

    def test_subject_marker_without_footer(self):
        self.assertEqual(publish.breaking_footer("No footer here."), "")
        self.assertIn("- feat!: drop x", publish.notes("v0.2.0", [("feat!: drop x", "")]))

    def test_none(self):
        self.assertIn("None.", publish.notes("v0.2.0", []))


class BreakingRecordGuard(unittest.TestCase):
    def setUp(self):
        self.calls = []
        self.original_run, self.original_output = publish.subprocess.run, publish.subprocess.check_output

    def tearDown(self):
        publish.subprocess.run, publish.subprocess.check_output = self.original_run, self.original_output

    def fake(self, changed, tag_message, describe=(0, "v0.1.0\n", ""), merged=(0, "", ""), shallow="false",
             kind="tag"):
        class Result:
            def __init__(self, returncode, stdout, stderr=""):
                self.returncode, self.stdout, self.stderr = returncode, stdout, stderr

        def run(args, **kwargs):
            if args[1] == "rev-parse":
                return Result(0, shallow + "\n")
            if args[1] == "describe":
                return Result(*describe)
            if args[1] == "tag":
                return Result(*merged)
            raise AssertionError("unexpected git call: " + " ".join(args))
        publish.subprocess.run = run

        def output(args, **kwargs):
            if args[1] == "diff":
                return changed
            if args[1] == "cat-file":
                return kind + "\n"
            return tag_message
        publish.subprocess.check_output = output

    def test_install_change_without_record_is_refused(self):
        self.fake("config/rbac/kustomization.yaml\nREADME.md\n", "v0.2.0\n")
        with self.assertRaisesRegex(ValueError, "BREAKING CHANGE"):
            publish.require_breaking_record("v0.2.0", [])

    def test_tag_message_records_none(self):
        self.fake("api/v1alpha1/agentrun_types.go\n", "v0.2.0\n\nBreaking-Change: none\n")
        publish.require_breaking_record("v0.2.0", [])

    def test_docs_only_release_needs_no_record(self):
        self.fake("docs/README.md\n", "v0.2.0\n")
        publish.require_breaking_record("v0.2.0", [])

    def test_a_footer_is_a_record(self):
        publish.require_breaking_record("v0.2.0", [("Move the ledger", "delete the old Role")])

    def test_subject_marker_alone_is_not_a_record(self):
        self.fake("config/manager/gateway.yaml\n", "v0.2.0\n")
        with self.assertRaisesRegex(ValueError, "BREAKING CHANGE"):
            publish.require_breaking_record("v0.2.0", [("feat!: move the gateway", "")])

    def test_example_change_needs_a_record(self):
        self.fake("examples/portable-workflows/kubernetes/owner.yaml\n", "v0.2.0\n")
        with self.assertRaisesRegex(ValueError, "examples/portable-workflows"):
            publish.require_breaking_record("v0.2.0", [])

    def test_lightweight_tag_is_refused_on_every_path(self):
        # %(contents) of a lightweight tag is the commit message, so a commit
        # body line must not stand in for the tag's record. The refusal comes
        # first, so a footer or a first release does not skip it.
        self.fake("config/rbac/kustomization.yaml\n", "fix: x\n\nBreaking-Change: none\n", kind="commit")
        with self.assertRaisesRegex(ValueError, "lightweight"):
            publish.require_annotated("v0.2.0")
        original = publish.breaking_changes
        publish.breaking_changes = lambda tag: [("feat: x", "delete the old Role")]
        try:
            with self.assertRaisesRegex(ValueError, "lightweight"):
                publish.checked_breaking_changes("v0.2.0")
        finally:
            publish.breaking_changes = original

    def test_annotated_tag_is_accepted(self):
        self.fake("README.md\n", "v0.2.0\n")
        publish.require_annotated("v0.2.0")

    def test_first_release_needs_no_record(self):
        self.fake("config/rbac/kustomization.yaml\n", "v0.1.0\n",
                  describe=(128, "", "fatal: No names found, cannot describe anything."), merged=(0, "", ""))
        publish.require_breaking_record("v0.1.0", [])
        self.assertIsNone(publish.previous_release("v0.1.0"))

    def test_describe_error_fails_closed(self):
        self.fake("config/rbac/kustomization.yaml\n", "v0.2.0\n",
                  describe=(128, "", "fatal: Not a valid object name v0.2.0^"),
                  merged=(129, "", "error: malformed object name v0.2.0^"))
        with self.assertRaisesRegex(ValueError, "cannot find the release before v0.2.0"):
            publish.require_breaking_record("v0.2.0", [])

    def test_describe_failure_with_earlier_tags_fails_closed(self):
        self.fake("config/rbac/kustomization.yaml\n", "v0.2.0\n",
                  describe=(128, "", "fatal: something went wrong"), merged=(0, "v0.1.0\n", ""))
        with self.assertRaisesRegex(ValueError, "cannot find the release before"):
            publish.require_breaking_record("v0.2.0", [])

    def test_shallow_checkout_fails_closed(self):
        self.fake("config/rbac/kustomization.yaml\n", "v0.2.0\n", shallow="true")
        with self.assertRaisesRegex(ValueError, "shallow"):
            publish.require_breaking_record("v0.2.0", [])

    def test_refusal_says_one_footer_covers_the_release(self):
        self.fake("config/rbac/kustomization.yaml\n", "v0.2.0\n")
        with self.assertRaisesRegex(ValueError, "one footer anywhere in the release"):
            publish.require_breaking_record("v0.2.0", [])


class ResumedDraft(unittest.TestCase):
    def setUp(self):
        self.originals = (publish.gh, publish.checked_breaking_changes, publish.subprocess.check_output)

    def tearDown(self):
        publish.gh, publish.checked_breaking_changes, publish.subprocess.check_output = self.originals

    def test_existing_draft_is_checked_before_upload(self):
        import json
        import tempfile
        calls = []

        class Result:
            returncode, stdout = 0, '{"isDraft": true}'

        def gh(*args, check=True):
            calls.append(args[:2])
            return Result()

        def refuse(tag):
            raise ValueError("no record")
        publish.gh = gh
        publish.checked_breaking_changes = refuse
        with tempfile.TemporaryDirectory() as directory:
            Path(directory, "metadata.json").write_text(json.dumps({"version": "0.2.0", "commit": "abc"}))
            Path(directory, "images.json").write_text("{}")
            publish.subprocess.check_output = lambda *a, **k: "abc\n"
            with self.assertRaisesRegex(ValueError, "no record"):
                publish.publish("v0.2.0", directory)
        self.assertEqual(calls, [("release", "view")])


if __name__ == "__main__":
    unittest.main()
