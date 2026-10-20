#!/usr/bin/env python3
"""Verify release inventory, hashes and the actual executable on its native OS."""

import argparse
import hashlib
import json
from pathlib import Path
import platform
import re
import subprocess
import tarfile
import tempfile
import zipfile


def verify(directory, version, native):
    root = Path(directory)
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(?:[-+.][0-9A-Za-z.-]+)?", version):
        raise ValueError("invalid version")
    targets = json.loads(Path(__file__).with_name("targets.json").read_text())
    expected = {f"sproozi_{version}_{t['os']}_{t['arch']}." + ("zip" if t["os"] == "windows" else "tar.gz") for t in targets}
    actual = {p.name for p in root.glob("sproozi_*") if p.name.endswith((".tar.gz", ".zip")) and not p.name.endswith(("_source.tar.gz", "_kubernetes.tar.gz"))}
    if actual != expected:
        raise ValueError(f"release target mismatch: missing {expected - actual}, extra {actual - expected}")
    sums = {}
    for line in (root / "checksums.txt").read_text().splitlines():
        digest, name = line.split(None, 1)
        name = name.lstrip(" *")
        if name in sums or Path(name).name != name or not re.fullmatch("[0-9a-f]{64}", digest):
            raise ValueError("invalid checksum entry")
        sums[name] = digest
    required = expected | {f"sproozi_{version}_source.tar.gz"}
    native_image = ""
    if (root / "images.json").exists():
        # Published images come with their release-matched Kubernetes assets.
        required |= {"images.json", f"sproozi_{version}_kubernetes.tar.gz"}
        native_image = json.loads((root / "images.json").read_text())["images"]["native"]["reference"]
    for name in required:
        if name not in sums or hashlib.sha256((root / name).read_bytes()).hexdigest() != sums[name]:
            raise ValueError(f"checksum mismatch: {name}")
    if not native:
        return
    system = {"Darwin": "darwin", "Linux": "linux", "Windows": "windows"}[platform.system()]
    arch = {"x86_64": "amd64", "AMD64": "amd64", "arm64": "arm64", "aarch64": "arm64"}[platform.machine()]
    name = f"sproozi_{version}_{system}_{arch}." + ("zip" if system == "windows" else "tar.gz")
    with tempfile.TemporaryDirectory(prefix="sproozi-install-") as temporary:
        if name.endswith(".zip"):
            with zipfile.ZipFile(root / name) as archive:
                if set(archive.namelist()) != {"sproozi.exe", "LICENSE"}:
                    raise ValueError("unexpected zip contents")
                archive.extractall(temporary)
        else:
            with tarfile.open(root / name) as archive:
                if {item.name for item in archive} != {"sproozi", "LICENSE"}:
                    raise ValueError("unexpected tar contents")
                archive.extractall(temporary, filter="data")
        binary = str(Path(temporary) / ("sproozi.exe" if system == "windows" else "sproozi"))
        output = subprocess.check_output([binary, "--version"], text=True)
        if not output.startswith("sproozi " + version + " (commit "):
            raise ValueError(f"wrong executable version: {output}")
        if native_image and "native image " + native_image + "\n" not in output:
            raise ValueError(f"executable does not embed the published native image: {output}")
        output = subprocess.check_output([binary, "--help"], text=True)
        if "Usage: sproozi COMMAND" not in output:
            raise ValueError("installed help failed")
        result = subprocess.run([binary, "not-a-command"], capture_output=True, text=True)
        if result.returncode == 0 or not result.stderr:
            raise ValueError("installed binary accepted invalid command")
    print(f"Verified {system}/{arch} {version}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--directory", default="dist")
    parser.add_argument("--version", required=True)
    parser.add_argument("--native", action="store_true")
    args = parser.parse_args()
    verify(args.directory, args.version, args.native)
