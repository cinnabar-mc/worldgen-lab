#!/usr/bin/env python3
"""Build the bridge from pinned upstream source and the lab's reviewed patches."""

import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parent
SOURCE = ROOT / ".steel"
REVISION = "da3fb73fc05b3114e24babe68b506c98a9185752"
REMOTE = "https://github.com/Steel-Foundation/SteelMC.git"


def git(*args, capture=False):
    """Run Git only in the build-owned source checkout."""
    return subprocess.run(
        ["git", "-C", str(SOURCE), *args], check=True,
        stdout=subprocess.PIPE if capture else None,
    ).stdout


def prepare():
    """Fetch the exact revision and apply patches, refusing unexpected local edits."""
    patches = b"\n".join(p.read_bytes() for p in sorted((ROOT / "patches").glob("*.patch")))
    identity = hashlib.sha256(REVISION.encode() + patches).hexdigest()
    if not SOURCE.exists():
        SOURCE.mkdir()
        git("init", "--quiet")
        git("remote", "add", "origin", REMOTE)
        git("fetch", "--depth", "1", "origin", REVISION)
        git("checkout", "--detach", "FETCH_HEAD")
    if git("rev-parse", "HEAD", capture=True).decode().strip() != REVISION:
        raise RuntimeError("native/.steel is not at the pinned upstream revision")
    if git("diff", "--binary", capture=True):
        raise RuntimeError("native/.steel has unexpected source edits; preserve them before rebuilding")
    metadata = SOURCE / ".git" / "worldgen-lab-patches.json"
    previous = json.loads(metadata.read_text()) if metadata.exists() else None
    staged = git("diff", "--cached", "--binary", capture=True).decode()
    if staged != (previous["diff"] if previous else ""):
        raise RuntimeError("native/.steel has unexpected staged source edits")
    if previous and previous["identity"] == identity:
        return
    old_patch = SOURCE / ".git" / "worldgen-lab-applied.patch"
    new_patch = SOURCE / ".git" / "worldgen-lab-next.patch"
    new_patch.write_bytes(patches)
    if previous:
        git("apply", "--reverse", "--index", str(old_patch))
    try:
        git("apply", "--index", str(new_patch))
    except subprocess.CalledProcessError:
        if previous:
            git("apply", "--index", str(old_patch))
        raise
    old_patch.write_bytes(patches)
    metadata.write_text(json.dumps({"identity": identity, "diff": git("diff", "--cached", "--binary", capture=True).decode()}))


def main():
    """Prepare dependencies, then forward Cargo arguments or run a locked release build."""
    prepare()
    args = sys.argv[1:]
    if args == ["--prepare-only"]:
        return
    command = args or ["build", "--locked", "--release"]
    subprocess.run(
        ["cargo", command[0], "--manifest-path", str(ROOT / "Cargo.toml"), *command[1:]],
        check=True,
        env={**os.environ, "RUSTFLAGS": os.environ.get("RUSTFLAGS", "-C target-cpu=native")},
    )


if __name__ == "__main__":
    main()
