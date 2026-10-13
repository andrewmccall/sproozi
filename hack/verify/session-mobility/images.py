#!/usr/bin/env python3
"""Load built mobility images using the existing canonical Kind digest verifier."""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cluster", required=True)
    parser.add_argument("--output-directory", type=Path, required=True)
    parser.add_argument("--hermes-tag", help="Also import a locally built/pulled stock Hermes tag, including registry prefix")
    args = parser.parse_args()
    if not re.fullmatch(r"sproozi-mobility-[a-z0-9-]+", args.cluster):
        raise ValueError("Use a dedicated mobility Kind cluster")
    os.umask(0o077)
    out = args.output_directory.resolve()
    out.mkdir(mode=0o700, parents=True, exist_ok=True)
    spec = importlib.util.spec_from_file_location("kind_images", Path(__file__).resolve().parents[1] / "orchestration-kind/build.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    module.OUT = out
    os.environ["KIND_CLUSTER"] = args.cluster
    images = {name: module.load("docker.io/sproozi-session/" + name + ":fixture") for name in ["native", "control"]}
    if args.hermes_tag:
        images["hermes"] = module.load(args.hermes_tag)
    (out / "images.json").write_text(json.dumps(images, indent=2) + "\n")
    print(json.dumps({name: value["image"] for name, value in images.items()}))


if __name__ == "__main__":
    main()
