#!/usr/bin/env python3
"""Prove the native UID's gateway-only fence in an isolated Docker namespace."""
import argparse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import subprocess
import threading
import uuid


def run(*args, check=True):
    return subprocess.run(args, check=check, capture_output=True, text=True, timeout=30)


class Fixture(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b"SPROOZI_GATEWAY_FENCE_ALLOWED")


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--image", required=True)
    parser.add_argument("--report", required=True)
    args = parser.parse_args()
    image = run("docker", "image", "inspect", args.image, "--format", "{{.Id}}").stdout.strip()
    resolved = run("docker", "run", "--rm", "--read-only", "--cap-drop", "ALL",
                   "--entrypoint", "/usr/bin/getent", image, "ahostsv4", "host.docker.internal").stdout
    gateway = resolved.split()[0]
    server = ThreadingHTTPServer(("0.0.0.0", 0), Fixture)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    port = server.server_address[1]
    name = "sproozi-network-proof-" + uuid.uuid4().hex
    container = None
    try:
        container = run("docker", "run", "--detach", "--name", name, "--user", "0:0",
                        "--read-only", "--cap-drop", "ALL", "--cap-add", "NET_ADMIN",
                        "--security-opt", "no-new-privileges", "--network", "bridge",
                        "--memory", "64m", "--pids-limit", "32", "--entrypoint",
                        "/usr/local/bin/sproozi-network", image, gateway, str(port)).stdout.strip()
        run("docker", "exec", container, "/usr/local/bin/sproozi-network", "check", gateway, str(port))

        def worker(binary, *command):
            return run("docker", "run", "--rm", "--network", "container:" + container,
                       "--read-only", "--cap-drop", "ALL", "--user", "1000:1000",
                       "--security-opt", "no-new-privileges", "--memory", "64m",
                       "--pids-limit", "32", "--entrypoint", binary, image, *command, check=False)

        allowed = worker("/usr/bin/curl", "--silent", "--show-error", "--fail", "--max-time", "5",
                         f"http://{gateway}:{port}/")
        assert allowed.returncode == 0 and allowed.stdout == "SPROOZI_GATEWAY_FENCE_ALLOWED", allowed.stderr
        denied = {}
        for label, url in {
            "otherPort": f"http://{gateway}:9/",
            "directIPv4": "http://1.1.1.1:80/",
            "directIPv6": "http://[2606:4700:4700::1111]:80/",
            "dns": "http://example.com/",
        }.items():
            result = worker("/usr/bin/curl", "--silent", "--show-error", "--fail", "--max-time", "3", url)
            assert result.returncode != 0, label
            denied[label] = result.returncode
        tamper = worker("/usr/sbin/iptables", "-F", "SPROOZI_NATIVE")
        assert tamper.returncode != 0, "native UID changed its fence"
        run("docker", "exec", container, "/usr/local/bin/sproozi-network", "check", gateway, str(port))
        with open(args.report, "w") as output:
            json.dump({"passed": True, "image": image, "gatewayIPv4": gateway,
                       "gatewayPort": port, "denied": denied, "nativeCannotAlterRules": True,
                       "limitations": "Network fence only; no session or accounting round trip"}, output, indent=2)
            output.write("\n")
    finally:
        if container:
            run("docker", "rm", "--force", container)
        server.shutdown()
        server.server_close()


if __name__ == "__main__":
    main()
