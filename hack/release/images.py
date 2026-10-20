#!/usr/bin/env python3
"""Build release images per platform, then merge them into a verified manifest.

`build` runs the pinned bake recipe for one platform. With --push it pushes by
digest and records the digest; without it the build only proves the recipe.
`index` merges the per-platform digests into one multi-platform reference per
image and writes images.json: the digests local setup and Kubernetes manifests
use, the client versions the recipe installed, the helper contract and a hash
of every recipe input.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess

ROOT = Path(__file__).resolve().parents[2]
IMAGES = {"native": "sproozi-native", "control": "sproozi"}
RECIPE = [
    "hack/release/docker-bake.hcl",
    "hack/demo/codex.Dockerfile",
    "internal/runtime/docker/image/clients.Dockerfile",
    "internal/runtime/docker/image/Dockerfile",
    "Dockerfile",
    "go.mod",
    "go.sum",
]
DIGEST = re.compile(r"^sha256:[0-9a-f]{64}$")


def recipe_hash():
    h = hashlib.sha256()
    paths = sorted(RECIPE + [p.relative_to(ROOT).as_posix() for p in (ROOT / "hack/clients").rglob("*") if p.is_file()])
    for path in paths:
        h.update(path.encode() + b"\0" + (ROOT / path).read_bytes() + b"\0")
    return h.hexdigest()


def clients():
    out = {}
    for name in ("codex", "claude-code"):
        manifest = json.loads((ROOT / "hack/clients" / name / "package.json").read_text())
        (package, version), = manifest["dependencies"].items()
        out[name] = {"package": package, "version": version}
    return out


def helper_contract():
    match = re.search(r'const contractVersion = "([0-9]+)"', (ROOT / "cmd/sproozi-network/probe.go").read_text())
    if not match:
        raise ValueError("helper contract version not found")
    return match.group(1)


def build(args):
    metadata = Path(args.output).with_suffix(".metadata.json")
    command = ["docker", "buildx", "bake", "-f", str(ROOT / "hack/release/docker-bake.hcl"),
               "--metadata-file", str(metadata), "--set", f"*.platform={args.platform}"]
    if args.push:
        for target, name in IMAGES.items():
            command += ["--set", f"{target}.output=type=image,name={args.repository}/{name},push-by-digest=true,name-canonical=true,push=true",
                        "--set", f"{target}.attest=type=provenance,mode=max", "--set", f"{target}.attest=type=sbom",
                        "--set", f"{target}.tags="]
    else:
        command += ["--set", "*.output=type=cacheonly"]
    subprocess.run(command + list(IMAGES), check=True, cwd=ROOT, env={**os.environ, "VERSION": args.version, "REPOSITORY": args.repository})
    result = {"platform": args.platform, "pushed": args.push, "digests": {}}
    if args.push:
        data = json.loads(metadata.read_text())
        for target in IMAGES:
            digest = data[target]["containerimage.digest"]
            if not DIGEST.match(digest):
                raise ValueError(f"{target} has no pushed digest")
            result["digests"][target] = digest
    Path(args.output).write_text(json.dumps(result, indent=2) + "\n")


def index(args):
    platforms = [json.loads(Path(p).read_text()) for p in args.digests]
    if not platforms or not all(p["pushed"] for p in platforms):
        raise ValueError("every platform must be pushed before indexing")
    manifest = {"version": args.version, "helperContract": helper_contract(), "recipeSHA256": recipe_hash(),
                "clients": clients(), "images": {}}
    for target, name in IMAGES.items():
        repository = f"{args.repository}/{name}"
        sources = [f"{repository}@{p['digests'][target]}" for p in platforms]
        tag = f"{repository}:{args.version}"
        subprocess.run(["docker", "buildx", "imagetools", "create", "--tag", tag, *sources], check=True)
        inspected = json.loads(subprocess.check_output(["docker", "buildx", "imagetools", "inspect", tag, "--format", "{{json .Manifest}}"], text=True))
        digest = inspected["digest"]
        if not DIGEST.match(digest):
            raise ValueError(f"{tag} has no index digest")
        listed = {f"{m['platform']['os']}/{m['platform']['architecture']}": m["digest"]
                  for m in inspected.get("manifests", []) if m.get("platform", {}).get("os") != "unknown"}
        expected = {p["platform"] for p in platforms}
        if set(listed) != expected:
            raise ValueError(f"{tag} platforms {sorted(listed)} differ from {sorted(expected)}")
        manifest["images"][target] = {"reference": f"{repository}@{digest}", "tag": tag, "platforms": listed}
    output = Path(args.output)
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n")
    print(manifest["images"]["native"]["reference"])


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    b = commands.add_parser("build")
    b.add_argument("--platform", required=True, choices=["linux/amd64", "linux/arm64"])
    b.add_argument("--repository", required=True)
    b.add_argument("--version", required=True)
    b.add_argument("--output", required=True)
    b.add_argument("--push", action="store_true")
    i = commands.add_parser("index")
    i.add_argument("--repository", required=True)
    i.add_argument("--version", required=True)
    i.add_argument("--output", required=True)
    i.add_argument("digests", nargs="+")
    args = parser.parse_args()
    {"build": build, "index": index}[args.command](args)
