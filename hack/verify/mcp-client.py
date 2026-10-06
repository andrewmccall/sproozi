#!/usr/bin/env python3
"""Check the actual renderer with stock Codex in a temporary CODEX_HOME."""
import json
import os
from pathlib import Path
import subprocess
import tempfile

root = Path(__file__).resolve().parents[2]
config = subprocess.check_output(["go", "run", "./hack/verify/mcp-config"], cwd=root, text=True)
with tempfile.TemporaryDirectory(prefix="sproozi-mcp-client-") as directory:
    Path(directory, "config.toml").write_text(config)
    env = dict(os.environ, CODEX_HOME=directory)
    version = subprocess.check_output(["codex", "--version"], env=env, text=True).strip()
    for name in ("docs", "inventory"):
        connection = json.loads(subprocess.check_output(["codex", "mcp", "get", name, "--json"], env=env, text=True))
        expected = f"https://sproozi-gateway.sproozi-system.svc:8443/mcp/{name}"
        assert connection["transport"]["url"] == expected, connection
        assert connection["enabled"] is True, connection
    print(f"PASS: {version} loaded both generated MCP connections; no model request")
