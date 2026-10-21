#!/usr/bin/env python3
"""Render release-matched Kubernetes assets with digest-pinned images.

The archive extracts to sproozi-release/:

  crds.yaml                     every Sproozi CRD, applied before anything else
  install/                      controller, gateway, RBAC, namespaces
  self-managed-ca/              optional certificate controller (no cert-manager)
  quickstart-self-managed-ca/   install + self-managed CA + kubernetes.read gateway
  quickstart-external-ca/       install + kubernetes.read gateway; you supply TLS
  first-check/                  provider-free AgentRun with permitted and denied paths
  cli-access/                   the CLI's ServiceAccount and Role for a remote executor's namespace
                                (sproozi-sessions by default; never sproozi-system)
  kind/                         a two-node Kind cluster configuration for trying the release
  README.md                     where to start

Nothing in the tree is modified; overlays are rendered in a scratch directory.
"""

import argparse
import io
import json
from pathlib import Path
import re
import shutil
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[2]
TEMPLATES = ROOT / "hack/release/kubernetes"
PINNED = re.compile(r"^[a-z0-9./_:-]+@sha256:[0-9a-f]{64}$")


def kustomize():
    subprocess.run(["make", "kustomize"], cwd=ROOT, check=True, capture_output=True)
    return str(ROOT / "bin/kustomize")


def build(binary, scratch, name, resources, control):
    overlay = scratch / name
    overlay.mkdir()
    relative = [str(Path("..", "..", "..", r)) for r in resources]
    (overlay / "kustomization.yaml").write_text(json.dumps({
        "apiVersion": "kustomize.config.k8s.io/v1beta1", "kind": "Kustomization", "resources": relative,
        # The bases already rename "controller" to their example image.
        "images": [{"name": base, "newName": control.split("@")[0], "digest": control.split("@")[1]}
                   for base in ("controller", "example.com/sproozi")]}))
    return subprocess.check_output([binary, "build", str(overlay)], text=True)


def split(documents):
    crds, rest = [], []
    for doc in re.split(r"(?m)^---\n", documents):
        if not doc.strip():
            continue
        (crds if re.search(r"(?m)^kind: CustomResourceDefinition$", doc) else rest).append(doc.rstrip() + "\n")
    return "---\n".join(crds), "---\n".join(rest)


def render(version, control, native, output):
    if not PINNED.match(control) or not PINNED.match(native):
        raise ValueError("control and native images must be repository@sha256:<digest>")
    binary = kustomize()
    # Overlays sit three levels below the repository root: .local/<tmp>/<name>.
    (ROOT / ".local").mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="release-k8s-", dir=ROOT / ".local") as temporary:
        scratch = Path(temporary)
        crds, install = split(build(binary, scratch, "install", ["config/install", "config/agent-rbac"], control))
        _, certificates = split(build(binary, scratch, "certificates", ["config/certificates"], control))
        _, cli_access = split(build(binary, scratch, "cli-access", ["config/cli-access"], control))
        expected = len(list((ROOT / "config/crd/bases").glob("*.yaml")))
        if crds.count("kind: CustomResourceDefinition") != expected:
            raise ValueError(f"expected exactly {expected} CRDs, one per file in config/crd/bases")
        for text, name in [(install, "install"), (certificates, "certificates")]:
            for image in re.findall(r"(?m)^\s+image: (\S+)$", text):
                if image != control:
                    raise ValueError(f"{name} references unpinned image {image}")
        out = scratch / "sproozi-release"
        out.mkdir()
        (out / "crds.yaml").write_text(crds)
        for directory, file, content in [("install", "install.yaml", install), ("self-managed-ca", "certificates.yaml", certificates),
                                         ("cli-access", "cli-access.yaml", cli_access)]:
            (out / directory).mkdir()
            (out / directory / file).write_text(content)
            (out / directory / "kustomization.yaml").write_text(f"apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- {file}\n")
        for template in TEMPLATES.iterdir():
            shutil.copytree(template, out / template.name)
        check = out / "first-check/resources.yaml"
        check.write_text(check.read_text().replace("SPROOZI_NATIVE_IMAGE", native))
        (out / "README.md").write_text(
            f"# Sproozi {version} Kubernetes assets\n\nFollow docs/guides/install-kubernetes.md for version {version}:\n"
            f"https://github.com/andrewmccall/sproozi/blob/v{version}/docs/guides/install-kubernetes.md\n")
        for name in ["quickstart-self-managed-ca", "quickstart-external-ca"]:
            subprocess.run([binary, "build", str(out / name)], check=True, capture_output=True)
        output = Path(output).resolve()
        output.parent.mkdir(parents=True, exist_ok=True)
        buffer = io.BytesIO()
        with tarfile.open(fileobj=buffer, mode="w:gz", format=tarfile.PAX_FORMAT) as archive:
            for path in sorted(out.rglob("*")):
                info = archive.gettarinfo(str(path), arcname=path.relative_to(scratch).as_posix())
                info.uid = info.gid = info.mtime = 0
                info.uname = info.gname = ""
                info.mode = 0o755 if path.is_dir() else 0o644
                with (path.open("rb") if path.is_file() else io.BytesIO()) as data:
                    archive.addfile(info, data if path.is_file() else None)
        output.write_bytes(buffer.getvalue())
    print(output)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    parser.add_argument("--control-image", required=True)
    parser.add_argument("--native-image", required=True)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    render(args.version, args.control_image, args.native_image, args.output)
