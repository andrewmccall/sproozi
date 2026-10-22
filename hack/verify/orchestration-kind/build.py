#!/usr/bin/env python3
"""Build pinned clients and retain the canonical Kind containerd digests."""
import hashlib
import json
import os
import re
import subprocess
import sys
import tempfile
import time
from concurrent.futures import ThreadPoolExecutor
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


def build_command(name, tag, dockerfile):
    """Return the image build command, with CI layer caching when requested."""
    cache = os.environ.get("SPROOZI_BUILD_CACHE", "")
    if cache == "gha":
        # The E2E workflow exposes the Actions cache to buildx. Inputs are
        # pinned by lockfile and base digest (ADR 0021), so cached layers
        # match a clean build of the same sources.
        return [
            "docker",
            "buildx",
            "build",
            "--load",
            "--cache-from",
            "type=gha,scope=" + name,
            "--cache-to",
            "type=gha,mode=max,scope=" + name,
            "-t",
            tag,
            "-f",
            dockerfile,
            ".",
        ]
    if cache:
        raise RuntimeError("SPROOZI_BUILD_CACHE must be empty or gha")
    return ["docker", "build", "-t", tag, "-f", dockerfile, "."]


def kind_nodes():
    cluster = os.environ["KIND_CLUSTER"]
    return output(
        os.environ.get("KIND", "kind"), "get", "nodes", "--name", cluster
    ).splitlines()


def import_archive(tag, nodes, platform):
    """Stream one docker save into every node's containerd import.

    Returns the SHA-256 of the archive. The archive is never written to disk,
    and each node receives exactly the bytes that were hashed.
    """
    save = subprocess.Popen(["docker", "save", tag], stdout=subprocess.PIPE)
    imports = []
    for node in nodes:
        log = tempfile.TemporaryFile()
        command = [
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
        ]
        process = subprocess.Popen(
            command, stdin=subprocess.PIPE, stdout=log, stderr=subprocess.STDOUT
        )
        imports.append((node, command, process, log))
    checksum = hashlib.sha256()
    try:
        while chunk := save.stdout.read(1 << 20):
            checksum.update(chunk)
            for _, _, process, _ in imports:
                process.stdin.write(chunk)
    except BrokenPipeError:
        pass
    finally:
        save.stdout.close()
        for _, _, process, _ in imports:
            try:
                process.stdin.close()
            except BrokenPipeError:
                pass
    failures = []
    if save.wait() != 0:
        failures.append("docker save " + tag + " exited " + str(save.returncode))
    for node, command, process, log in imports:
        if process.wait() != 0:
            log.seek(0)
            failures.append(
                " ".join(command)
                + " exited "
                + str(process.returncode)
                + ": "
                + log.read().decode(errors="replace")
            )
        log.close()
    if failures:
        raise RuntimeError("Kind image import failed: " + "; ".join(failures))
    return checksum.hexdigest()


def resolve_on_node(tag, node):
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
                return digest, canonical, {"resolvedRepoDigests": repo_digests}
        time.sleep(0.2)
    raise RuntimeError(
        "Kind CRI could not resolve canonical manifest " + canonical + " on " + node
    )


def load(tag):
    nodes = kind_nodes()
    platform = "linux/" + output(
        "docker", "image", "inspect", tag, "--format", "{{.Architecture}}"
    )
    checksum = import_archive(tag, nodes, platform)
    with ThreadPoolExecutor(max_workers=len(nodes)) as pool:
        resolved = list(pool.map(lambda node: resolve_on_node(tag, node), nodes))
    digests = [digest for digest, _, _ in resolved]
    if len(set(digests)) != 1:
        raise RuntimeError("Kind nodes disagree on canonical image manifest")
    digest, canonical, _ = resolved[-1]
    return {
        "image": canonical,
        "dockerTag": tag,
        "archiveSHA256": checksum,
        "manifestDigest": digest,
        "nodes": nodes,
        "criResolution": {
            node: resolution for node, (_, _, resolution) in zip(nodes, resolved)
        },
    }


def pull_on_nodes(reference):
    """Have every Kind node pull the pinned Hermes image itself, in parallel.

    The reference is pinned by digest, so each node's containerd verifies the
    exact manifest. This skips a host pull, save and import of a large image.
    """
    if not re.fullmatch(r"[a-z0-9./_-]+@sha256:[0-9a-f]{64}", reference):
        raise RuntimeError("Hermes must be pinned by digest: " + reference)
    nodes = kind_nodes()

    def pull(node):
        run("docker", "exec", node, "crictl", "pull", reference, capture_output=True)
        inspected = json.loads(
            output("docker", "exec", node, "crictl", "inspecti", reference)
        )
        repo_digests = inspected["status"]["repoDigests"]
        if reference not in repo_digests:
            raise RuntimeError(
                "Kind CRI could not resolve " + reference + " on " + node
            )
        return {"resolvedRepoDigests": repo_digests}

    with ThreadPoolExecutor(max_workers=len(nodes)) as pool:
        resolved = list(pool.map(pull, nodes))
    return {
        "image": reference,
        "manifestDigest": reference.rsplit("@", 1)[1],
        "pulledByNodes": True,
        "nodes": nodes,
        "criResolution": dict(zip(nodes, resolved)),
    }


def pulled_hermes(reference):
    """Return pull-hermes's record for this cluster's nodes, or pull now."""
    record = OUT / "hermes.json"
    if record.exists():
        pulled = json.loads(record.read_text())
        if pulled["image"] == reference and pulled["nodes"] == kind_nodes():
            return pulled
    return pull_on_nodes(reference)


def hermes_stamp(reference, path):
    """Read a provenance file from the Hermes image on a Kind node."""
    node = kind_nodes()[0]
    name = "sproozi-stamp-" + hashlib.sha256(path.encode()).hexdigest()[:12]
    return json.loads(
        output(
            "docker", "exec", node, "ctr", "-n", "k8s.io", "run", "--rm",
            reference, name, "/bin/cat", path,
        )
    )


IMAGES = {
    "provider": "hack/verify/orchestration-kind/provider/Dockerfile",
    "clients": "hack/verify/orchestration-kind/clients.Dockerfile",
    # Native supervisor, checkpoint helper and remote executor for the
    # managed-session case (managed.py).
    "native": "hack/verify/orchestration-kind/native.Dockerfile",
}


def acceptance_tag(name):
    return "docker.io/sproozi-kind/" + name + ":acceptance"


def prepare(name, source_snapshot):
    """Build an acceptance image and return its tag."""
    tag = acceptance_tag(name)
    dockerfile = IMAGES[name]
    log = OUT / (name + "-build.log")
    with log.open("w") as stream:
        run(
            *build_command(name, tag, dockerfile),
            stdout=stream,
            stderr=subprocess.STDOUT,
        )
    require_unchanged_sources(source_snapshot)
    return tag


def inspect_versions():
    versions = {}
    observed = {}
    for name, binary in [
        ("codex", "codex"),
        ("claude-code", "claude"),
        ("opencode", "opencode"),
        ("copilot", "copilot"),
    ]:
        version = output(
            "docker",
            "run",
            "--rm",
            "--network",
            "none",
            "--env",
            "HOME=/tmp",
            acceptance_tag("clients"),
            binary,
            "--version",
        )
        match = re.search(r"\b(\d+\.\d+\.\d+)\b", version)
        if not match:
            raise RuntimeError("Unexpected native " + name + " version: " + version)
        observed[name] = version
        versions[name] = match.group(1)
    return versions, observed


def write_images(loaded, built):
    images = dict(loaded)
    hermes = built["hermes"]
    stamp = hermes_stamp(hermes, "/etc/hermes/image-provenance.json")
    install_stamp = hermes_stamp(hermes, "/opt/hermes/install-stamp.json")
    images["hermes"]["upstreamImage"] = hermes
    images["hermes"]["sourceCommit"] = install_stamp["commit"]
    images["buildSourceSHA256"] = built["buildSourceSHA256"]
    images["versions"] = dict(built["versions"], hermes=install_stamp["displayVersion"])
    images["observedVersions"] = dict(
        built["observedVersions"],
        hermes={"installStamp": install_stamp, "imageProvenance": stamp},
    )
    require_unchanged_sources(built["buildSourceSHA256"])
    images["sourceSnapshotVerified"] = True
    (OUT / "images.json").write_text(json.dumps(images, indent=2) + "\n")
    print(
        "Built and loaded pinned upstream clients; provenance: "
        + str(OUT / "images.json")
    )


def start():
    OUT.mkdir(parents=True, exist_ok=True)
    # Snapshot before Docker reads its contexts. Never attribute a compiled
    # image to files that were edited while the build or import was in flight.
    source_snapshot = build_source_hashes()
    (OUT / "build-source-snapshot.json").write_text(
        json.dumps(source_snapshot, indent=2) + "\n"
    )
    (OUT / "images.json").unlink(missing_ok=True)
    (OUT / "built.json").unlink(missing_ok=True)
    # Official coordinator and bounded worker use the same pinned upstream image.
    hermes = os.environ.get("SPROOZI_HERMES_IMAGE") or HERMES_IMAGE
    return source_snapshot, hermes


def built_record(source_snapshot, hermes):
    versions, observed = inspect_versions()
    return {
        "hermes": hermes,
        "buildSourceSHA256": source_snapshot,
        "versions": versions,
        "observedVersions": observed,
    }


def main(argv):
    """Build, load or both.

    build: build the images, needing no cluster; writes built.json.
    pull-hermes: have each KIND_CLUSTER node pull the pinned Hermes image;
    writes hermes.json. load: import the images that build recorded, and use
    pull-hermes's record when it matches these nodes (otherwise pull now).
    Without an argument, each image is loaded as soon as it is ready.
    """
    mode = argv[0] if argv else "all"
    if mode not in ["all", "build", "load", "pull-hermes"]:
        raise SystemExit("usage: build.py [build|load|pull-hermes]")
    if mode == "pull-hermes":
        hermes = os.environ.get("SPROOZI_HERMES_IMAGE") or HERMES_IMAGE
        OUT.mkdir(parents=True, exist_ok=True)
        (OUT / "hermes.json").write_text(json.dumps(pull_on_nodes(hermes), indent=2) + "\n")
        return
    if mode == "load":
        built = json.loads((OUT / "built.json").read_text())
        require_unchanged_sources(built["buildSourceSHA256"])
        with ThreadPoolExecutor(max_workers=len(IMAGES) + 1) as pool:
            futures = {
                name: pool.submit(load, acceptance_tag(name)) for name in IMAGES
            }
            futures["hermes"] = pool.submit(pulled_hermes, built["hermes"])
            loaded = {name: future.result() for name, future in futures.items()}
        write_images(loaded, built)
        return
    source_snapshot, hermes = start()
    # The builds, the Hermes pull and each image's import are independent;
    # every image still passes the same digest and CRI checks.
    with ThreadPoolExecutor(max_workers=len(IMAGES) + 1) as pool:
        if mode == "build":
            futures = {
                name: pool.submit(prepare, name, source_snapshot)
                for name in IMAGES
            }
        else:
            futures = {
                name: pool.submit(
                    lambda n: load(prepare(n, source_snapshot)), name
                )
                for name in IMAGES
            }
            futures["hermes"] = pool.submit(pull_on_nodes, hermes)
        results = {name: future.result() for name, future in futures.items()}
    built = built_record(source_snapshot, hermes)
    if mode == "build":
        (OUT / "built.json").write_text(json.dumps(built, indent=2) + "\n")
        print("Built pinned upstream clients: " + str(OUT / "built.json"))
        return
    write_images(results, built)


if __name__ == "__main__":
    main(sys.argv[1:])
