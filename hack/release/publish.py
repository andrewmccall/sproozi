#!/usr/bin/env python3
"""Stage verified assets as a draft, check remote hashes, then publish once."""

import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import tempfile


def gh(*args, check=True):
    return subprocess.run(["gh", *args], check=check, capture_output=True, text=True)


def publish(tag, directory):
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?", tag):
        raise ValueError("expected a release tag such as v0.1.0")
    root = Path(directory)
    metadata = json.loads((root / "metadata.json").read_text())
    commit = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    if metadata["version"] != tag[1:] or metadata["commit"] != commit:
        raise ValueError("assets do not match this tag and commit")
    if not (root / "images.json").exists():
        raise ValueError("a release must include the published image manifest images.json")
    assets = sorted(p for p in root.iterdir() if p.name.endswith((".tar.gz", ".zip")) or p.name in {"checksums.txt", "metadata.json", "images.json"})
    existing = gh("release", "view", tag, "--json", "isDraft", check=False)
    if existing.returncode == 0:
        if not json.loads(existing.stdout)["isDraft"]:
            raise ValueError("refusing to replace a published release; use a new version")
    else:
        gh("release", "create", tag, "--verify-tag", "--draft", "--title", tag, "--notes", "Sproozi CLI for macOS, Linux and Windows.\n\nInstall with `brew install andrewmccall/tap/sproozi` or download an archive below. Verify its SHA-256 with checksums.txt. Homebrew checks for new releases each hour.\n\nThe CLI requires separate home or Kubernetes setup. Agent workers run in Linux containers. See https://github.com/andrewmccall/sproozi/blob/" + tag + "/docs/guides/install.md.\n\nStandalone binaries are unsigned. Native Windows host boundaries are tested in CI; Docker Desktop and interactive-console acceptance are separate checks.")
    gh("release", "upload", tag, *map(str, assets), "--clobber")
    with tempfile.TemporaryDirectory(prefix="sproozi-published-") as temporary:
        gh("release", "download", tag, "--dir", temporary)
        downloaded = {p.name: p for p in Path(temporary).iterdir()}
        if set(downloaded) != {p.name for p in assets}:
            raise ValueError("draft asset inventory differs from the verified release")
        for asset in assets:
            if hashlib.sha256(asset.read_bytes()).digest() != hashlib.sha256(downloaded[asset.name].read_bytes()).digest():
                raise ValueError("draft hash mismatch: " + asset.name)
    arguments = ["release", "edit", tag, "--draft=false"]
    if "-" in tag:
        arguments.append("--prerelease")
    gh(*arguments)
    print(gh("release", "view", tag, "--json", "url", "--jq", ".url").stdout.strip())


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--directory", default="dist")
    args = parser.parse_args()
    publish(args.tag, args.directory)
