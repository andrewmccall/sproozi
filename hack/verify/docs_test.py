#!/usr/bin/env python3
"""Offline tests for the ADR reference check in docs.py."""
from pathlib import Path
import importlib.util
import tempfile
import unittest

spec = importlib.util.spec_from_file_location("docs_check", Path(__file__).with_name("docs.py"))
docs_check = importlib.util.module_from_spec(spec)
spec.loader.exec_module(docs_check)

README = """# Architecture decisions

## Decisions

| Decision | Topic |
| --- | --- |
| [0001](0001-first.md) | First. |

## Reserved numbers

| Number | Record |
| --- | --- |
| 0055 | In flight. |

## Removed

| Number | Title |
| --- | --- |
| 0042 | Gone. |
"""


class MentionForms(unittest.TestCase):
    def test_forms(self):
        text = (
            "ADR 0001, ADRs 0003 and 0017, ADR 0041, 0045 and ADR0004/0009.\n"
            "ADRs 0008-0010, ADRs 0009–0012, ADR\n0014 and ADR 0002, 2026."
        )
        self.assertEqual(
            docs_check.adr_mentions(text),
            {"0001", "0003", "0017", "0041", "0045", "0004", "0009", "0008",
             "0010", "0012", "0014", "0002"},
        )

    def test_ignores_non_records(self):
        self.assertEqual(docs_check.adr_mentions("ADR is fine; version 0042 too"), set())

    def test_tables_hold_reserved_and_removed_only(self):
        self.assertEqual(docs_check.adr_table_numbers(README), {"0055", "0042"})


class Resolution(unittest.TestCase):
    def check(self, doc: str) -> list[str]:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            (root / "docs/adr").mkdir(parents=True)
            (root / "docs/adr/README.md").write_text(README, encoding="utf-8")
            (root / "docs/adr/0001-first.md").write_text("# 0001\n", encoding="utf-8")
            (root / "docs/guide.md").write_text(doc, encoding="utf-8")
            (root / ".claude").mkdir()
            (root / ".claude/note.md").write_text("ADR 0099", encoding="utf-8")
            return docs_check.check_adr_references(root)

    def test_known_numbers_pass(self):
        self.assertEqual(self.check("ADR 0001, ADR 0042 and ADRs 0055 and 0001."), [])

    def test_unknown_number_fails(self):
        problems = self.check("See ADR 0001 and ADR 0077.")
        self.assertEqual(len(problems), 1)
        self.assertIn("docs/guide.md: ADR 0077", problems[0])

    def test_unknown_number_in_a_list_fails(self):
        self.assertEqual(len(self.check("ADRs 0001 and 0078.")), 1)


if __name__ == "__main__":
    unittest.main()
