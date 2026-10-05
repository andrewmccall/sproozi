#!/usr/bin/env python3
"""Check release inputs controlled by this repository."""

from pathlib import Path
import re
import sys

ROOT = Path(__file__).resolve().parents[2]


def fail(message: str) -> None:
    print(f"release verification failed: {message}", file=sys.stderr)
    raise SystemExit(1)


def main() -> None:
    dockerfile = ROOT / "Dockerfile"
    text = dockerfile.read_text()
    for line in text.splitlines():
        if line.startswith("FROM ") and line.split()[1].lower() != "scratch":
            if not re.search(r"@sha256:[0-9a-f]{64}(?:\s|$)", line):
                fail(f"unpinned base in {dockerfile.relative_to(ROOT)}: {line}")
    if "FROM scratch" not in text:
        fail("controller final stage must be scratch")

    sample = (ROOT / "config/samples/sproozi_v1alpha1_agentruntime.yaml").read_text()
    if not re.search(r"image:\s+ghcr\.io/openai/codex-universal@sha256:[0-9a-f]{64}", sample):
        fail("sample AgentRuntime must use a digest-pinned vanilla Codex image")
    print("release input verification passed (offline checks only)")


if __name__ == "__main__":
    main()
