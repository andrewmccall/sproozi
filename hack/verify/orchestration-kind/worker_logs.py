"""Bounded acceptance diagnostics; never participate in worker completion."""

import json
import re
import subprocess
import threading
import time


class WorkerLogCapture:
    def __init__(self, namespace, directory, max_bytes=65536):
        self.namespace = namespace
        self.directory = directory
        self.max_bytes = max_bytes
        self.stopping = threading.Event()
        self.lock = threading.Lock()
        self.records = {}
        self.processes = []
        self.followers = []
        self.watcher = threading.Thread(target=self._watch, daemon=True)

    def start(self):
        self.watcher.start()

    def _watch(self):
        while not self.stopping.is_set():
            try:
                result = subprocess.run(
                    ["kubectl", "get", "pods", "-n", self.namespace, "-o", "json"],
                    text=True,
                    capture_output=True,
                    timeout=5,
                    check=False,
                )
                if result.returncode == 0:
                    for pod in json.loads(result.stdout)["items"]:
                        metadata = pod["metadata"]
                        labels = metadata.get("labels", {})
                        uid = labels.get("sproozi.com/agentrun-uid", "")
                        if not re.fullmatch(r"[a-zA-Z0-9-]+", uid):
                            continue
                        with self.lock:
                            if uid in self.records:
                                continue
                            path = self.directory / ("worker-" + uid + ".log")
                            self.records[uid] = {
                                "runUID": uid,
                                "runName": labels.get("sproozi.com/agentrun"),
                                "pod": metadata["name"],
                                "path": str(path),
                                "maxBytes": self.max_bytes,
                                "capturedBytes": 0,
                                "truncated": False,
                            }
                        thread = threading.Thread(
                            target=self._follow,
                            args=(uid, metadata["name"], path),
                            daemon=True,
                        )
                        self.followers.append(thread)
                        thread.start()
            except (OSError, ValueError, subprocess.TimeoutExpired):
                pass
            self.stopping.wait(0.2)

    def _follow(self, uid, pod, path):
        # Start while the Pod is being created and retry readiness errors. Once
        # the log stream is established it survives API Pod cleanup long enough
        # to retain a fast native process failure. This observer owns no Pod.
        size = 0
        with path.open("wb") as output:
            path.chmod(0o600)
            while not self.stopping.is_set():
                process = subprocess.Popen(
                    [
                        "kubectl",
                        "logs",
                        "-n",
                        self.namespace,
                        pod,
                        "-c",
                        "agent",
                        "--follow",
                        "--timestamps",
                        "--pod-running-timeout=5s",
                    ],
                    stdout=subprocess.PIPE,
                    stderr=subprocess.STDOUT,
                )
                with self.lock:
                    self.processes.append(process)
                while not self.stopping.is_set():
                    chunk = process.stdout.read1(4096)
                    if not chunk:
                        break
                    remaining = self.max_bytes - size
                    if remaining:
                        output.write(chunk[:remaining])
                        output.flush()
                        size += min(len(chunk), remaining)
                    with self.lock:
                        self.records[uid]["capturedBytes"] = size
                        self.records[uid]["truncated"] |= len(chunk) > remaining
                if self.stopping.is_set() and process.poll() is None:
                    process.terminate()
                try:
                    code = process.wait(timeout=3)
                except subprocess.TimeoutExpired:
                    process.kill()
                    code = process.wait(timeout=3)
                # A complete native log stream exits successfully. Readiness
                # failures are retried; a deleted Pod cannot supply more logs.
                if code == 0:
                    return
                existing = subprocess.run(
                    ["kubectl", "get", "pod", pod, "-n", self.namespace, "-o", "name"],
                    text=True,
                    capture_output=True,
                    timeout=5,
                    check=False,
                )
                if existing.returncode != 0:
                    return
                self.stopping.wait(0.2)

    def snapshot(self):
        with self.lock:
            return [dict(record) for record in self.records.values()]

    def stop(self):
        self.stopping.set()
        with self.lock:
            processes = list(self.processes)
        for process in processes:
            if process.poll() is None:
                process.terminate()
        self.watcher.join(timeout=6)
        until = time.monotonic() + 6
        for follower in self.followers:
            follower.join(timeout=max(0, until - time.monotonic()))
