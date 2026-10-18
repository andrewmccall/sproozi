#!/usr/bin/env python3
"""Build pinned clients and retain the canonical Kind containerd digests."""
import hashlib
import json
import os
import re
import subprocess
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
OUT = ROOT / ".local/verification/orchestration-kind"
# Hermes 0.22.2. Bump with the steps in hack/clients/README.md; the weekly
# latest-clients workflow sets SPROOZI_HERMES_IMAGE to test the newest image.
HERMES_IMAGE = "docker.io/nousresearch/hermes-agent@sha256:36099926da316742423544e055d4734777105ffe6341873af5459b957d0c2919"  # noqa: E501


def run(*args, **kw):
    kw.setdefault("check", True)
    kw.setdefault("text", True)
    return subprocess.run(args, **kw)


def output(*args):
    return run(*args, capture_output=True).stdout.strip()


def build_source_hashes():
    paths = (
        list((ROOT / "hack/verify/orchestration-kind/provider").glob("*.go"))
        + list((ROOT / "hack/verify/orchestration-kind").rglob("*Dockerfile"))
        + list((ROOT / "hack/clients").glob("*/package*.json"))
        + [ROOT / "hack/clients/install.sh"]
        + [Path(__file__), ROOT / "go.mod", ROOT / "go.sum", ROOT / ".dockerignore"]
    )
    return {
        str(path.relative_to(ROOT)): hashlib.sha256(path.read_bytes()).hexdigest()
        for path in sorted(paths)
    }


def require_unchanged_sources(expected):
    current = build_source_hashes()
    changed = [
        name
        for name in sorted(set(expected) | set(current))
        if expected.get(name) != current.get(name)
    ]
    if changed:
        raise RuntimeError(
            "Acceptance build source changed during compilation: " + ", ".join(changed)
        )


def load(tag):
    archive = OUT / (tag.split("/")[-1].replace(":", "-") + ".tar")
    run("docker", "save", "-o", str(archive), tag)
    checksum = hashlib.file_digest(archive.open("rb"), "sha256").hexdigest()
    cluster = os.environ["KIND_CLUSTER"]
    nodes = output(
        os.environ.get("KIND", "kind"), "get", "nodes", "--name", cluster
    ).splitlines()
    digests = []
    cri_resolution = {}
    for node in nodes:
        platform = "linux/" + output(
            "docker", "image", "inspect", tag, "--format", "{{.Architecture}}"
        )
        with archive.open("rb") as stream:
            run(
                "docker",
                "exec",
                "-i",
                node,
                "ctr",
                "-n",
                "k8s.io",
                "images",
                "import",
                "--platform",
                platform,
                "--digests",
                "-",
                stdin=stream,
                capture_output=True,
            )
        rows = output(
            "docker", "exec", node, "ctr", "-n", "k8s.io", "images", "ls"
        ).splitlines()
        digest = next(line.split()[2] for line in rows if line.split()[0] == tag)
        canonical = tag.rsplit(":", 1)[0] + "@" + digest
        run(
            "docker",
            "exec",
            node,
            "ctr",
            "-n",
            "k8s.io",
            "images",
            "tag",
            "--force",
            tag,
            canonical,
            capture_output=True,
        )
        # CRI normalizes digest references to repository@digest, removing any
        # tag. A ctr alias containing tag@digest exists but cannot be resolved
        # by Kubernetes ImageStatus; validate the actual consumer on each node.
        until = time.monotonic() + 15
        while time.monotonic() < until:
            inspected = run(
                "docker",
                "exec",
                node,
                "crictl",
                "inspecti",
                canonical,
                capture_output=True,
                check=False,
            )
            if inspected.returncode == 0:
                repo_digests = json.loads(inspected.stdout)["status"]["repoDigests"]
                if canonical in repo_digests:
                    cri_resolution[node] = {"resolvedRepoDigests": repo_digests}
                    break
            time.sleep(1)
        else:
            raise RuntimeError(
                "Kind CRI could not resolve canonical manifest "
                + canonical
                + " on "
                + node
            )
        digests.append(digest)
    if len(set(digests)) != 1:
        raise RuntimeError("Kind nodes disagree on canonical image manifest")
    archive.unlink()
    return {
        "image": canonical,
        "dockerTag": tag,
        "archiveSHA256": checksum,
        "manifestDigest": digest,
        "nodes": nodes,
        "criResolution": cri_resolution,
    }


def main():
    OUT.mkdir(parents=True, exist_ok=True)
    # Snapshot before Docker reads its contexts. Never attribute a compiled
    # image to files that were edited while the build or import was in flight.
    source_snapshot = build_source_hashes()
    (OUT / "build-source-snapshot.json").write_text(
        json.dumps(source_snapshot, indent=2) + "\n"
    )
    (OUT / "images.json").unlink(missing_ok=True)
    images = {}
    for name, dockerfile in [
        ("provider", "hack/verify/orchestration-kind/provider/Dockerfile"),
        ("clients", "hack/verify/orchestration-kind/clients.Dockerfile"),
    ]:
        tag = "docker.io/sproozi-kind/" + name + ":acceptance"
        log = OUT / (name + "-build.log")
        with log.open("w") as stream:
            run(
                "docker",
                "build",
                "--pull",
                "--no-cache",
                "-t",
                tag,
                "-f",
                dockerfile,
                ".",
                stdout=stream,
                stderr=subprocess.STDOUT,
            )
        require_unchanged_sources(source_snapshot)
        images[name] = load(tag)
    # Official coordinator and bounded worker use the same pinned upstream image.
    hermes = os.environ.get("SPROOZI_HERMES_IMAGE") or HERMES_IMAGE
    run("docker", "pull", hermes)
    run("docker", "tag", hermes, "docker.io/sproozi-kind/hermes:acceptance")
    images["hermes"] = load("docker.io/sproozi-kind/hermes:acceptance")
    images["hermes"]["upstreamImage"] = hermes
    images["buildSourceSHA256"] = source_snapshot
    images["versions"] = {}
    observed = {}
    for name, binary in [
        ("codex", "codex"),
        ("claude-code", "claude"),
        ("opencode", "opencode"),
    ]:
        version = output(
            "docker",
            "run",
            "--rm",
            "--network",
            "none",
            "--env",
            "HOME=/tmp",
            "docker.io/sproozi-kind/clients:acceptance",
            binary,
            "--version",
        )
        match = re.search(r"\b(\d+\.\d+\.\d+)\b", version)
        if not match:
            raise RuntimeError("Unexpected native " + name + " version: " + version)
        observed[name] = version
        images["versions"][name] = match.group(1)
    stamp = json.loads(
        output(
            "docker",
            "run",
            "--rm",
            "--network",
            "none",
            "--entrypoint",
            "/bin/cat",
            hermes,
            "/etc/hermes/image-provenance.json",
        )
    )
    install_stamp = json.loads(
        output(
            "docker",
            "run",
            "--rm",
            "--network",
            "none",
            "--entrypoint",
            "/bin/cat",
            hermes,
            "/opt/hermes/install-stamp.json",
        )
    )
    images["versions"]["hermes"] = install_stamp["displayVersion"]
    images["hermes"]["sourceCommit"] = install_stamp["commit"]
    observed["hermes"] = {"installStamp": install_stamp, "imageProvenance": stamp}
    images["observedVersions"] = observed
    require_unchanged_sources(source_snapshot)
    images["sourceSnapshotVerified"] = True
    (OUT / "images.json").write_text(json.dumps(images, indent=2) + "\n")
    print(
        "Built and loaded pinned upstream clients; provenance: "
        + str(OUT / "images.json")
    )


if __name__ == "__main__":
    main()
