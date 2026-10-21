#!/usr/bin/env python3
"""Stage verified assets as a draft, check remote hashes, then publish once."""

import argparse
import hashlib
import json
from pathlib import Path
import re
import subprocess
import tempfile


def gh(*args, check=True):
    return subprocess.run(["gh", *args], check=check, capture_output=True, text=True)


BREAKING_SUBJECT = re.compile(r"^[a-z]+(?:\([^)]*\))?!:")


def breaking_changes(tag):
    """Commit subjects marked breaking since the previous release tag.

    A commit is breaking when its subject uses "type!:" or its body has a
    "BREAKING CHANGE:" line, as docs/contributing/releases.md asks.
    """
    previous = previous_release(tag)
    span = tag if previous is None else previous + ".." + tag
    log = subprocess.check_output(["git", "log", "--format=%s%x00%b%x1e", span], text=True)
    found = []
    for entry in log.split("\x1e"):
        subject, _, body = entry.strip().partition("\x00")
        footer = breaking_footer(body)
        if subject and (BREAKING_SUBJECT.match(subject) or footer):
            found.append((subject, footer))
    return found


def previous_release(tag):
    """The nearest earlier v* tag reachable from tag, or None for the first release.

    "No earlier tag" is told apart from a failure: a shallow checkout, an
    unknown tag or any other git error raises, so the guard fails closed
    rather than treating the release as the first.
    """
    shallow = subprocess.run(["git", "rev-parse", "--is-shallow-repository"], capture_output=True, text=True)
    if shallow.returncode != 0 or shallow.stdout.strip() != "false":
        raise ValueError("cannot find the previous release in a shallow or unreadable checkout;"
                         " fetch the full history (fetch-depth: 0)")
    previous = subprocess.run(["git", "describe", "--tags", "--abbrev=0", "--match", "v*", tag + "^"],
                              capture_output=True, text=True)
    if previous.returncode == 0:
        return previous.stdout.strip()
    # describe failed: it is the first release only if git can list the tags
    # merged into the parent and none of them is a v* tag.
    merged = subprocess.run(["git", "tag", "--list", "v*", "--merged", tag + "^"], capture_output=True, text=True)
    if merged.returncode == 0 and not merged.stdout.strip():
        return None
    raise ValueError("cannot find the release before " + tag + ": " +
                     (previous.stderr.strip() or merged.stderr.strip() or "git describe failed"))


def breaking_footer(body):
    """The text of a "BREAKING CHANGE:" footer, up to the next trailer or the end.

    The footer carries what an operator must do, so it is published whole.
    """
    match = re.search(r"(?ms)^BREAKING CHANGE:[ \t]*(.*?)(?=^[A-Za-z-]+: |\Z)", body)
    return match.group(1).strip() if match else ""


# Paths whose change usually needs operator action on upgrade: the API types,
# every install manifest and the examples operators apply beside them.
COMPATIBILITY_PATHS = ("api/", "config/", "examples/")


def require_breaking_record(tag, breaking):
    """Refuse a release that changes the API or install with no recorded break.

    A "BREAKING CHANGE:" footer, or "Breaking-Change: none" in an annotated
    tag's message, is the record. A "type!:" subject alone is not, because it
    publishes no operator steps. The guard exists because a missing footer
    publishes "None" and an operator then skips the upgrade steps.

    Any one footer in the release span satisfies the guard for every touched
    path: it checks that a record exists, not that it covers each change.
    """
    if any(footer for _, footer in breaking):
        return
    previous = previous_release(tag)
    if previous is None:
        return  # the first release has nothing to compare against
    changed = subprocess.check_output(["git", "diff", "--name-only", previous, tag], text=True).split()
    touched = sorted({path for path in changed if path.startswith(COMPATIBILITY_PATHS)})
    if not touched:
        return
    message = subprocess.check_output(["git", "tag", "-l", "--format=%(contents)", tag], text=True)
    if re.search(r"(?mi)^Breaking-Change:\s*none\s*$", message):
        return
    raise ValueError("this release changes " + ", ".join(touched[:5]) + (" and more" if len(touched) > 5 else "") +
                     " but no commit in the release has a BREAKING CHANGE: footer saying what operators must do"
                     " (one footer anywhere in the release satisfies this check, so make it cover every change);"
                     " add one, or put 'Breaking-Change: none' in the annotated tag message")


def require_annotated(tag):
    """Refuse a lightweight tag on every path, before any other check.

    A lightweight tag has no message of its own: %(contents) would read the
    tagged commit's message, so a commit body could stand in for the
    'Breaking-Change: none' record, and the release would carry no tagger.
    """
    kind = subprocess.check_output(["git", "cat-file", "-t", tag], text=True).strip()
    if kind != "tag":
        raise ValueError(tag + " is a lightweight tag; release from an annotated tag (git tag -a)")


def checked_breaking_changes(tag):
    require_annotated(tag)
    breaking = breaking_changes(tag)
    require_breaking_record(tag, breaking)
    return breaking


def notes(tag, breaking):
    def item(subject, footer):
        if not footer:
            return "- " + subject
        return "- " + subject + "\n\n" + "\n".join("  " + line if line else "" for line in footer.splitlines())
    section = "\n".join(item(subject, footer) for subject, footer in breaking) if breaking else "None."
    return ("## Breaking changes\n\n" + section + "\n\nSee https://github.com/andrewmccall/sproozi/blob/" + tag +
            "/docs/contributing/releases.md#versioning-and-compatibility.\n\n## Install\n\n"
            "Sproozi CLI for macOS, Linux and Windows.\n\nInstall with `brew install andrewmccall/tap/sproozi` or download an archive below. Verify its SHA-256 with checksums.txt. Homebrew checks for new releases each hour.\n\nThe CLI requires separate home or Kubernetes setup. Agent workers run in Linux containers. See https://github.com/andrewmccall/sproozi/blob/" + tag + "/docs/guides/install.md.\n\nStandalone binaries are unsigned. Native Windows host boundaries are tested in CI; Docker Desktop and interactive-console acceptance are separate checks.")


def publish(tag, directory):
    if not re.fullmatch(r"v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?", tag):
        raise ValueError("expected a release tag such as v0.1.0")
    root = Path(directory)
    metadata = json.loads((root / "metadata.json").read_text())
    commit = subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip()
    if metadata["version"] != tag[1:] or metadata["commit"] != commit:
        raise ValueError("assets do not match this tag and commit")
    if not (root / "images.json").exists():
        raise ValueError("a release must include the published image manifest images.json")
    assets = sorted(p for p in root.iterdir() if p.name.endswith((".tar.gz", ".zip")) or p.name in {"checksums.txt", "metadata.json", "images.json"})
    existing = gh("release", "view", tag, "--json", "isDraft", check=False)
    if existing.returncode == 0 and not json.loads(existing.stdout)["isDraft"]:
        raise ValueError("refusing to replace a published release; use a new version")
    # Check before touching any draft, and rewrite a resumed draft's notes, so a
    # draft left by an earlier attempt cannot publish without the record.
    body = notes(tag, checked_breaking_changes(tag))
    if existing.returncode == 0:
        gh("release", "edit", tag, "--notes", body)
    else:
        gh("release", "create", tag, "--verify-tag", "--draft", "--title", tag, "--notes", body)
    gh("release", "upload", tag, *map(str, assets), "--clobber")
    with tempfile.TemporaryDirectory(prefix="sproozi-published-") as temporary:
        gh("release", "download", tag, "--dir", temporary)
        downloaded = {p.name: p for p in Path(temporary).iterdir()}
        if set(downloaded) != {p.name for p in assets}:
            raise ValueError("draft asset inventory differs from the verified release")
        for asset in assets:
            if hashlib.sha256(asset.read_bytes()).digest() != hashlib.sha256(downloaded[asset.name].read_bytes()).digest():
                raise ValueError("draft hash mismatch: " + asset.name)
    arguments = ["release", "edit", tag, "--draft=false"]
    if "-" in tag:
        arguments.append("--prerelease")
    gh(*arguments)
    print(gh("release", "view", tag, "--json", "url", "--jq", ".url").stdout.strip())


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--tag", required=True)
    parser.add_argument("--directory", default="dist")
    args = parser.parse_args()
    publish(args.tag, args.directory)
