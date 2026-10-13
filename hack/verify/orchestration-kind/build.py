#!/usr/bin/env python3
"""Build real pinned clients and retain the canonical Kind containerd digests."""
import hashlib
import json
import os
import re
import subprocess
import time
from pathlib import Path

ROOT = Path(__file__).resolve().parents[3]
OUT = ROOT / ".local/verification/orchestration-kind"


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
    # Official coordinator and bounded worker use the same pinned native release.
    hermes = "nousresearch/hermes-agent@sha256:9774f4f39a9bb8c2f68ce728ed5e99ddbad282163be56764afacf88ed952b784"
    run("docker", "pull", hermes)
    run("docker", "tag", hermes, "docker.io/sproozi-kind/hermes:acceptance")
    images["hermes"] = load("docker.io/sproozi-kind/hermes:acceptance")
    images["hermes"].update(
        {
            "upstreamImage": hermes,
            "upstreamVersion": "0.21.6",
            "sourceCommit": "818c13be1dc4fd28987e1e881a9408224afd4535",
        }
    )
    images["buildSourceSHA256"] = source_snapshot
    images["versions"] = {
        "codex": "0.161.0",
        "claude-code": "2.1.27",
        "opencode": "1.15.1",
        "hermes": "0.21.6",
    }
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
        if not match or match.group(1) != images["versions"][name]:
            raise RuntimeError("Unexpected native " + name + " version: " + version)
        observed[name] = version
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
    if (
        install_stamp["baseVersion"] != images["versions"]["hermes"]
        or install_stamp["displayVersion"] != images["versions"]["hermes"]
        or install_stamp["commit"] != images["hermes"]["sourceCommit"]
        or stamp["revision"] != install_stamp["commit"]
    ):
        raise RuntimeError(
            "Official Hermes install stamp disagrees with selected release"
        )
    # The image-provenance marker declares 0.0.0 in this upstream image; retain
    # that observed metadata without treating it as the CLI release version.
    observed["hermes"] = {"installStamp": install_stamp, "imageProvenance": stamp}
    images["observedVersions"] = observed
    require_unchanged_sources(source_snapshot)
    images["sourceSnapshotVerified"] = True
    (OUT / "images.json").write_text(json.dumps(images, indent=2) + "\n")
    print(
        "Built and loaded pinned acceptance images; provenance: "
        + str(OUT / "images.json")
    )


if __name__ == "__main__":
    main()
