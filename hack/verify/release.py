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
    check_workflows()
    check_go_builders()
    print("release input verification passed (offline checks only)")


def check_workflows() -> None:
    """ADR 0021: workflows execute only inputs pinned in this repository.

    This is a line matcher, not a YAML parser. A local action (`uses: ./...`)
    is exempt; a `repository:` checkout must have a 40-hex `ref:` within its
    step. If a new workflow shape trips it, extend the matcher rather than
    weakening a pattern. goreleaser is pinned by tag, not by checksum.
    """
    for workflow in sorted((ROOT / ".github/workflows").glob("*.yml")):
        name = workflow.relative_to(ROOT)
        if workflow.name == "latest-clients.yml":
            continue  # the canary deliberately tests unpinned latest clients
        lines = workflow.read_text().splitlines()
        for number, line in enumerate(lines, 1):
            stripped = line.split("#", 1)[0].strip()
            action = re.sub(r"^-\s+", "", stripped)
            if action.startswith("uses:") and not action.startswith("uses: ./") \
                    and not re.search(r"@[0-9a-f]{40}$", action):
                fail(f"{name}:{number}: action not pinned to a commit: {stripped}")
            if re.fullmatch(r"version:\s*latest", stripped):
                fail(f"{name}:{number}: tool version must be pinned, not latest")
            if re.fullmatch(r"check-latest:\s*true", stripped):
                fail(f"{name}:{number}: setup-go must not float to the latest patch")
            if stripped.startswith("repository:"):
                block = "\n".join(lines[number:number + 6])
                if not re.search(r"ref:\s*[0-9a-f]{40}\b", block):
                    fail(f"{name}:{number}: checkout of another repository must pin ref to a commit")



def check_go_builders() -> None:
    """Every Go builder image must match the toolchain CI and lint use.

    A release built with an older Go keeps standard-library vulnerabilities
    that `make verify-deps` reports fixed.
    """
    makefile = (ROOT / "Makefile").read_text()
    match = re.search(r"^LINT_TOOLCHAIN \?= go(\S+)$", makefile, re.M)
    if not match:
        fail("Makefile has no LINT_TOOLCHAIN pin")
    want = match.group(1)
    files = [p for p in ROOT.rglob("*") if p.is_file() and (p.name.endswith("Dockerfile") or p.name.endswith(".sh"))
             and not any(part.startswith(".") for part in p.relative_to(ROOT).parts)]
    for path in sorted(files):
        for number, line in enumerate(path.read_text(errors="replace").splitlines(), 1):
            for image in re.findall(r"golang:([0-9][^@\s]*)@", line):
                if image.split("-")[0] != want:
                    fail(f"{path.relative_to(ROOT)}:{number}: golang:{image} does not match LINT_TOOLCHAIN go{want}")


if __name__ == "__main__":
    main()
