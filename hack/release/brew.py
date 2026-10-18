#!/usr/bin/env python3
"""Build and test the tap's formula against the staged source release."""

import argparse
import hashlib
import importlib.util
import json
from pathlib import Path
import subprocess


root = Path.cwd()
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--assets", type=Path, default=root / "dist")
parser.add_argument("--tap-source", type=Path, default=root / ".local/tap")
args = parser.parse_args()
version = json.loads((args.assets / "metadata.json").read_text())["version"]
source = (args.assets / f"sproozi_{version}_source.tar.gz").resolve()
spec = importlib.util.spec_from_file_location("tap_formula", args.tap_source / "formula.py")
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
formula = module.render(version, source.as_uri(), hashlib.sha256(source.read_bytes()).hexdigest())
subprocess.run(["brew", "tap", "andrewmccall/tap", str(args.tap_source.resolve())], check=True)
tap = Path(subprocess.check_output(["brew", "--repo", "andrewmccall/tap"], text=True).strip())
(tap / "Formula").mkdir(exist_ok=True)
(tap / "Formula/sproozi.rb").write_text(formula)
subprocess.run(["brew", "install", "--build-from-source", "andrewmccall/tap/sproozi"], check=True)
subprocess.run(["brew", "test", "andrewmccall/tap/sproozi"], check=True)
subprocess.run(["sproozi", "--version"], check=True)
