#!/usr/bin/env python3
"""Small offline documentation gate for local links and private-path leaks."""
from pathlib import Path
import re
import sys

root = Path(__file__).resolve().parents[2]
errors = []


def anchor_names(text: str) -> set[str]:
    """Return the GitHub-style anchors emitted by Markdown headings."""
    anchors = set()
    for heading in re.findall(r"^#{1,6}\s+(.+?)\s*#*\s*$", text, re.MULTILINE):
        slug = re.sub(r"[^a-z0-9 -]", "", heading.lower())
        slug = re.sub(r"\s+", "-", slug).strip("-")
        anchors.add(slug)
    return anchors


required_docs = {
    "docs/README.md", "docs/explanations/system-architecture.md",
    "docs/explanations/capabilities-and-gateways.md",
    "docs/explanations/run-lifecycle.md", "docs/reference/agent-contract.md",
    "docs/reference/client-compatibility.md", "docs/guides/manual-pr-demo.md",
    "docs/adr/README.md", "docs/adr/0001-mcp-capabilities-through-shared-gateway.md",
    "docs/reference/kubernetes-mcp.md", "docs/reference/configured-mcp.md",
    "docs/guides/getting-started.md",
    ".agents/skills/verify-sproozi/SKILL.md",
    ".agents/skills/verify-sproozi/features/README.md",
    ".agents/skills/verify-sproozi/features/dependencies.md",
    ".agents/skills/verify-sproozi/features/demo.md",
    ".agents/skills/verify-sproozi/features/exploration.md",
}
for required in required_docs:
    if not (root / required).is_file():
        errors.append(f"missing required public guide/reference: {required}")

# The primary demo is deliberately usable without the optional monitoring and
# webhook integrations. Keep this assertion against the source kustomization,
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
    public_skill = path.is_relative_to(root / ".agents/skills/verify-sproozi")
    if path.name == "AGENTS.md" or any(part in {".git", ".local", ".claude", ".engineering", ".codex"} for part in path.parts) or (".agents" in path.parts and not public_skill) or "docs/impl" in str(path.relative_to(root)):
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
if errors:
    print("\n".join(errors), file=sys.stderr)
    raise SystemExit(1)
print("Documentation checks passed")
