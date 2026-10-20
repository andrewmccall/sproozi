#!/usr/bin/env python3
"""Bundle the selected Git revision and vendored modules for an offline brew build."""

import argparse
import gzip
import io
import json
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile


def bundle(revision, version, output, native_image=""):
    if not re.fullmatch(r"[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?", version):
        raise ValueError("version must be a release version without the v prefix")
    commit = subprocess.check_output(["git", "rev-parse", "--verify", revision + "^{commit}"], text=True).strip()
    date = subprocess.check_output(["git", "show", "-s", "--format=%cI", commit], text=True).strip()
    archive = subprocess.check_output(["git", "archive", "--format=tar", commit])
    output = Path(output).resolve()
    output.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="sproozi-release-") as temporary:
        root = Path(temporary)
        with tarfile.open(fileobj=io.BytesIO(archive)) as source:
            source.extractall(root, filter="data")
        subprocess.run(["go", "mod", "vendor"], cwd=root, check=True)
        (root / "release.json").write_text(json.dumps({"version": version, "commit": commit, "date": date}) + "\n")
        if native_image:
            # Source builds, including Homebrew's, embed the published image.
            if not re.fullmatch(r"[a-z0-9./_:-]+@sha256:[0-9a-f]{64}", native_image):
                raise ValueError("native image must be digest-pinned")
            (root / "internal/localsetup/release-images.json").write_text(json.dumps({"native": native_image}, indent=2) + "\n")
        with output.open("wb") as raw, gzip.GzipFile(filename="", mode="wb", fileobj=raw, mtime=0) as compressed:
            with tarfile.open(fileobj=compressed, mode="w", format=tarfile.PAX_FORMAT) as target:
                for path in sorted(root.rglob("*")):
                    info = target.gettarinfo(str(path), arcname="sproozi-" + version + "/" + path.relative_to(root).as_posix())
                    info.uid = info.gid = info.mtime = 0
                    info.uname = info.gname = ""
                    info.pax_headers = {}
                    if info.isfile():
                        with path.open("rb") as data:
                            target.addfile(info, data)
                    else:
                        target.addfile(info)
    print(output)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--revision", default="HEAD")
    parser.add_argument("--version", required=True)
    parser.add_argument("--output", required=True)
    parser.add_argument("--native-image", default="", help="published digest-pinned native image")
    args = parser.parse_args()
    bundle(args.revision, args.version, args.output, args.native_image)
