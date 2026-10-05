#!/usr/bin/env python3
"""Render the SRE demo with caller-owned immutable values."""

import argparse
import re
import subprocess
from pathlib import Path


IMAGE_PATTERN = re.compile(r"^[A-Za-z0-9./_:-]+@sha256:[a-f0-9]{64}$")
REPOSITORY_PATTERN = re.compile(r"^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$")
IMAGE_PLACEHOLDER = "ghcr.io/openai/codex-universal@sha256:" + ("a" * 64)
REPOSITORY_PLACEHOLDER = "example/sproozi-demo"


def replace_exact(document: str, old: str, new: str, expected: int) -> str:
    count = document.count(old)
    if count != expected:
        raise SystemExit(f"expected {expected} occurrence(s) of {old!r}, found {count}")
    return document.replace(old, new)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--codex-image", required=True)
    parser.add_argument("--repository", required=True)
    parser.add_argument("--model-auth", choices=("api_key", "chatgpt"), default="api_key")
    args = parser.parse_args()
    if not IMAGE_PATTERN.fullmatch(args.codex_image):
        raise SystemExit("--codex-image must be repository@sha256:<64 lowercase hex characters>")
    if not REPOSITORY_PATTERN.fullmatch(args.repository):
        raise SystemExit("--repository must be owner/name")

    root = Path(__file__).resolve().parents[2]
    rendered = subprocess.run(
        [str(root / "bin" / "kustomize"), "build", str(root / "examples" / "sre-demo")],
        cwd=root,
        check=True,
        capture_output=True,
        text=True,
    ).stdout
    rendered = replace_exact(rendered, IMAGE_PLACEHOLDER, args.codex_image, 1)
    rendered = replace_exact(rendered, REPOSITORY_PLACEHOLDER, args.repository, 3)
    if args.model_auth == "chatgpt":
        # ChatGPT is not API dollar billing. Retain token admission/accounting,
        # with room for stock Codex's repeated context; omit the API-dollar cap.
        rendered = replace_exact(rendered, "maxCostMicros: 500000", "maxCostMicros: 0", 1)
        rendered = replace_exact(rendered, "maxUnits: 100000", "maxUnits: 300000", 1)
    print(rendered, end="")


if __name__ == "__main__":
    main()
