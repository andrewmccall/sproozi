#!/usr/bin/env python3
"""Validate the required assertion manifest and optionally emit a redacted report.

This is deliberately an offline declaration gate.  It never turns an assertion
declaration into runtime evidence; live protocol and cluster gates must append
their own results in a later environment.
"""
from __future__ import annotations

import argparse
import json
import platform
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
EXPECTED = {
    prefix: count
    for prefix, count in (
        ("API", 5), ("ID", 7), ("K8S", 7), ("POD", 5), ("GH", 10),
        ("PKG", 7), ("NET", 8), ("MOD", 5), ("RES", 6), ("LIFE", 8),
        ("DOC", 4), ("PUB", 4),
    )
}


def commit() -> str:
    try:
        return subprocess.run(
            ["git", "rev-parse", "HEAD"], cwd=ROOT, check=True,
            capture_output=True, text=True,
        ).stdout.strip()
    except (OSError, subprocess.CalledProcessError):
        return "unknown"


def validate(path: Path) -> list[dict[str, str]]:
    try:
        document = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise ValueError(f"cannot read manifest: {exc}") from exc
    if document.get("version") != 1 or not isinstance(document.get("assertions"), list):
        raise ValueError("manifest must contain version 1 and an assertions list")
    assertions = document["assertions"]
    seen: set[str] = set()
    for item in assertions:
        if not isinstance(item, dict):
            raise ValueError("each assertion must be an object")
        identifier = item.get("id", "")
        if not isinstance(identifier, str) or "-" not in identifier:
            raise ValueError(f"invalid assertion id: {identifier!r}")
        prefix, number = identifier.rsplit("-", 1)
        if prefix not in EXPECTED or not number.isdigit() or int(number) < 1:
            raise ValueError(f"unknown assertion id: {identifier}")
        if identifier in seen:
            raise ValueError(f"duplicate assertion id: {identifier}")
        seen.add(identifier)
        for field in ("domain", "description", "expected_decision", "forbidden_side_effects"):
            if not isinstance(item.get(field), str) or not item[field].strip():
                raise ValueError(f"{identifier} missing non-empty {field}")
    for prefix, count in EXPECTED.items():
        actual = sorted(
            int(identifier.rsplit("-", 1)[1])
            for identifier in seen if identifier.startswith(prefix + "-")
        )
        expected = list(range(1, count + 1))
        if actual != expected:
            raise ValueError(f"{prefix} assertions must enumerate {expected}, got {actual}")
    if len(seen) != sum(EXPECTED.values()):
        raise ValueError(f"unexpected assertion count: {len(seen)}")
    return assertions


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--manifest", type=Path, default=ROOT / "test/acceptance/cases.json")
    parser.add_argument("--report", type=Path)
    args = parser.parse_args()
    try:
        assertions = validate(args.manifest)
    except ValueError as exc:
        print(f"Assertion manifest invalid: {exc}", file=sys.stderr)
        return 1
    if args.report:
        report = {
            "schema_version": 1,
            "kind": "offline-assertion-manifest",
            "generated_at": datetime.now(timezone.utc).isoformat(),
            "commit": commit(),
            "platform": platform.platform(),
            "status": "manifest_validated",
            "note": "Declared assertions are not runtime evidence; unexecuted assertions remain unverified.",
            "assertions": [
                {"id": item["id"], "domain": item["domain"], "status": "unverified"}
                for item in assertions
            ],
        }
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    print(f"Assertion manifest valid: {len(assertions)} individually named assertions")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
