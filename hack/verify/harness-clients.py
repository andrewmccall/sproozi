#!/usr/bin/env python3
"""Load the production MCP renderer with stock CLIs in disposable client state.

No model requests. No host configuration or credentials enter these processes.
"""
import argparse
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[2]


def execute(command, env, cwd):
    return subprocess.check_output(command, env=env, cwd=cwd, text=True, stderr=subprocess.PIPE, timeout=60)


def isolated_env(directory):
    home = Path(directory, "home")
    home.mkdir()
    env = {"PATH": os.environ["PATH"], "HOME": str(home), "TMPDIR": directory,
           "XDG_CONFIG_HOME": str(home / "config"), "XDG_DATA_HOME": str(home / "data"),
           "XDG_CACHE_HOME": str(home / "cache"), "XDG_STATE_HOME": str(home / "state"),
           "CODEX_HOME": str(home / ".codex"), "CLAUDE_CONFIG_DIR": str(home / ".claude"),
           "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
           "OPENCODE_DISABLE_MODELS_FETCH": "true", "OPENCODE_DISABLE_AUTOUPDATE": "true",
           "OPENCODE_DISABLE_PROJECT_CONFIG": "true", "OPENCODE_DISABLE_CLAUDE_CODE": "true",
           "OPENCODE_DISABLE_EXTERNAL_SKILLS": "true", "OPENCODE_DISABLE_DEFAULT_PLUGINS": "true"}
    return env


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--harness", action="append", choices=("codex", "claude-code", "opencode"))
    args = parser.parse_args()
    for selected in args.harness or ["codex", "claude-code", "opencode"]:
        binary = "claude" if selected == "claude-code" else selected
        if not shutil.which(binary):
            raise SystemExit(f"Install stock {binary} before running this gate")
        config = subprocess.check_output(["go", "run", "./hack/verify/mcp-config", "--harness", selected], cwd=ROOT, text=True)
        with tempfile.TemporaryDirectory(prefix="sproozi-harness-") as directory:
            env = isolated_env(directory)
            version = execute([binary, "--version"], env, directory).strip()
            if selected == "codex":
                Path(env["CODEX_HOME"]).mkdir()
                Path(env["CODEX_HOME"], "config.toml").write_text(config)
                for name in ("docs", "inventory"):
                    value = json.loads(execute([binary, "mcp", "get", name, "--json"], env, directory))
                    assert value["transport"]["url"] == f"https://sproozi-gateway.sproozi-system.svc:8443/mcp/{name}", value
                    assert value["enabled"] is True, value
            elif selected == "claude-code":
                Path(directory, ".mcp.json").write_text(config)
                for name in ("docs", "inventory"):
                    value = execute([binary, "mcp", "get", name], env, directory)
                    assert f"https://sproozi-gateway.sproozi-system.svc:8443/mcp/{name}" in value, value
                    assert "Type: http" in value, value
            else:
                path = Path(directory, "approved.json")
                path.write_text(config)
                env["OPENCODE_CONFIG"] = str(path)
                value = json.loads(execute([binary, "--pure", "debug", "config"], env, directory))
                assert value["mcp"] == json.loads(config)["mcp"], value["mcp"]
            print(f"PASS: {selected} {version} loaded both generated connections in disposable state; no model request")


if __name__ == "__main__":
    main()
