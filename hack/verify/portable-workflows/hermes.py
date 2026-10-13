"""Drive the deployed stock Hermes CLI; it owns discovery and MCP invocation."""
import json
import subprocess


def invoke(*, directory, kubeconfig, deployment, marker, intent):
    directory.mkdir(mode=0o700)
    command = ["kubectl", "--kubeconfig", str(kubeconfig.resolve()), "exec", "-i",
               "-n", "sproozi-coordinators", "deployment/" + deployment, "--",
               "/opt/hermes/bin/hermes", "chat", "--oneshot", "--query-file", "-", "--format", "stream-json",
               "--provider", "custom:fixture", "--model", "fixture-model", "--max-turns", "12",
               "--ignore-rules", "--toolsets", "sproozi-work"]
    with (directory / "stdout.jsonl").open("w") as stdout, (directory / "stderr.log").open("w") as stderr:
        result = subprocess.run(command, input=marker + json.dumps(intent), text=True,
                                stdout=stdout, stderr=stderr, timeout=150)
    if result.returncode:
        raise RuntimeError("Stock Hermes failed; inspect its private stderr.log and stdout.jsonl")
    events = [json.loads(line) for line in (directory / "stdout.jsonl").read_text().splitlines() if line.strip()]
    final = [event for event in events if event.get("type") == "result"]
    if len(final) != 1 or final[0].get("exit_code") != 0:
        raise RuntimeError("Stock Hermes did not produce one successful native result")
    prefix = "SPROOZI_WORK_OBSERVED "
    text = final[0].get("text", "")
    if not text.startswith(prefix):
        raise RuntimeError("Hermes did not consume the actual work MCP response")
    return json.loads(text[len(prefix):])
