#!/usr/bin/env python3
"""Small offline documentation gate for local links and private-path leaks."""
from pathlib import Path
import re
import subprocess
import sys

root = Path(__file__).resolve().parents[2]


def anchor_names(text: str) -> set[str]:
    """Return the GitHub-style anchors emitted by Markdown headings."""
    anchors = set()
    for heading in re.findall(r"^#{1,6}\s+(.+?)\s*#*\s*$", text, re.MULTILINE):
        slug = re.sub(r"[^a-z0-9 -]", "", heading.lower())
        slug = re.sub(r"\s+", "-", slug).strip("-")
        anchors.add(slug)
    return anchors


EXCLUDED_PARTS = {".git", ".local", ".claude", ".engineering", ".codex"}
ADR_SUFFIXES = {".md", ".go", ".py", ".yaml", ".yml", ".sh"}

# "ADR 0043", "ADRs 0003 and 0017", "ADR 0041, 0045", "ADR0004/0009" and
# "ADRs 0008-0010" (both ends of a range). Continuation numbers must start
# with 0 so a year after a comma is not read as a record. A mention split
# across a line break is caught; one split by a comment marker is not.
ADR_MENTION = re.compile(
    r"\bADRs?\s*(0\d{3})((?:\s*(?:,|/|&|-|\u2013|and|or|to)\s*0\d{3})*)"
)


def adr_mentions(text: str) -> set[str]:
    """Return every four-digit record number a text names as an ADR."""
    found = set()
    for match in ADR_MENTION.finditer(text):
        found.add(match.group(1))
        found.update(re.findall(r"0\d{3}", match.group(2)))
    return found


def adr_table_numbers(readme: str) -> set[str]:
    """Return the numbers in the Reserved numbers and Removed tables."""
    numbers = set()
    section = ""
    for line in readme.splitlines():
        if line.startswith("## "):
            section = line[3:].strip().lower()
        elif section in {"reserved numbers", "removed"}:
            match = re.match(r"\|\s*(\d{4})\s*\|", line)
            if match:
                numbers.add(match.group(1))
    return numbers


def repository_files(root: Path) -> list[Path]:
    """Return tracked files, or every file when git is unavailable."""
    try:
        listing = subprocess.run(
            ["git", "-C", str(root), "ls-files", "-z"],
            check=True, capture_output=True,
        ).stdout.decode("utf-8").split("\0")
        return [root / name for name in listing if name]
    except (OSError, subprocess.CalledProcessError):
        return [path for path in root.rglob("*") if path.is_file()]


def check_adr_references(root: Path) -> list[str]:
    """Report ADR mentions that name no record, reserved number or removal."""
    adr_dir = root / "docs/adr"
    readme = adr_dir / "README.md"
    known = {path.name[:4] for path in adr_dir.glob("[0-9][0-9][0-9][0-9]-*.md")}
    if readme.is_file():
        known |= adr_table_numbers(readme.read_text(encoding="utf-8"))
    problems = []
    for path in repository_files(root):
        rel = path.relative_to(root)
        if path.suffix not in ADR_SUFFIXES or not path.is_file():
            continue
        if any(part in EXCLUDED_PARTS for part in rel.parts) or rel.as_posix().startswith("docs/impl"):
            continue
        try:
            text = path.read_text(encoding="utf-8")
        except UnicodeDecodeError:
            continue
        for number in sorted(adr_mentions(text) - known):
            problems.append(
                f"{rel}: ADR {number} has no record in docs/adr and no row in the "
                "Reserved numbers or Removed tables of docs/adr/README.md"
            )
    return problems


def main() -> None:
    errors = []
    required_docs = {
        "docs/README.md", "docs/explanations/system-architecture.md",
        "docs/explanations/capabilities-and-gateways.md",
        "docs/explanations/run-lifecycle.md", "docs/reference/agent-contract.md",
        "docs/reference/client-compatibility.md", "docs/guides/manual-pr-demo.md",
        "docs/adr/README.md", "docs/adr/0001-mcp-capabilities-through-shared-gateway.md",
        "docs/reference/kubernetes-mcp.md", "docs/reference/configured-mcp.md",
        "docs/guides/pr-demo.md", "docs/guides/install-kubernetes.md",
        ".agents/skills/verify-sproozi/SKILL.md",
        ".agents/skills/verify-sproozi/features/README.md",
        ".agents/skills/verify-sproozi/features/dependencies.md",
        ".agents/skills/verify-sproozi/features/demo.md",
        ".agents/skills/verify-sproozi/features/exploration.md",
    }
    for required in required_docs:
        if not (root / required).is_file():
            errors.append(f"missing required public guide/reference: {required}")

    # The primary demo is deliberately usable without the optional monitoring
    # integration. Keep this assertion against the source kustomization,
    # rather than relying on a rendered manifest that may have been produced from
    # a different checkout.
    demo_kustomization = root / "examples/sre-demo/kustomization.yaml"
    if demo_kustomization.is_file():
        demo_text = demo_kustomization.read_text(encoding="utf-8").lower()
        for optional in ("alertmanager", "prometheus", "metrics"):
            if optional in demo_text:
                errors.append(
                    "examples/sre-demo/kustomization.yaml: base manual demo "
                    f"must not require optional {optional} integration"
                )
    else:
        errors.append("missing demo kustomization: examples/sre-demo/kustomization.yaml")

    for path in root.rglob("*.md"):
        # Filter on the path inside the repository so a checkout under a hidden
        # directory (for example a .claude worktree) is still checked.
        rel = path.relative_to(root)
        public_skill = rel.is_relative_to(".agents/skills/verify-sproozi")
        if path.name == "AGENTS.md" or any(part in {".git", ".local", ".claude", ".engineering", ".codex"} for part in rel.parts) or (".agents" in rel.parts and not public_skill) or "docs/impl" in str(rel):
            continue
        text = path.read_text(encoding="utf-8")
        for raw_target in re.findall(r"\[[^]]+\]\(([^)]+)\)", text):
            target, _, anchor = raw_target.partition("#")
            target = target.strip()
            if target.startswith(("http://", "https://", "mailto:")):
                continue
            destination = (path.parent / target).resolve()
            if not destination.exists():
                errors.append(f"{path.relative_to(root)}: missing link {target}")
            elif anchor and destination.suffix.lower() == ".md":
                destination_text = destination.read_text(encoding="utf-8")
                if anchor not in anchor_names(destination_text):
                    errors.append(f"{path.relative_to(root)}: missing anchor {target}#{anchor}")
        # Public docs may cite implementation paths in code spans. Catch stale
        # source references without treating ordinary shell commands as paths.
        for source in re.findall(r"`((?:api|cmd|config|internal|test|hack|agent)/[^`\s]+)`", text):
            source_path = root / source.rstrip(".,")
            if not source_path.exists() and not (root / (source.rstrip(".,") + ".go")).exists():
                errors.append(f"{path.relative_to(root)}: missing source reference {source}")
        for private in ("docs/PRD.md", "docs/implementation-plan.md", "docs/impl/"):
            if private in text:
                errors.append(f"{path.relative_to(root)}: private path reference {private}")
    errors.extend(check_adr_references(root))
    if errors:
        print("\n".join(errors), file=sys.stderr)
        raise SystemExit(1)
    print("Documentation checks passed")


if __name__ == "__main__":
    main()
