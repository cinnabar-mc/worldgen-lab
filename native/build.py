#!/usr/bin/env python3
"""Build the bridge from pinned upstream SteelMC source."""

import os
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parent
SOURCE = ROOT / ".steel"
REVISION = "de3147acbcca942888e97ac06a71c4c00be933d1"
REMOTE = "https://github.com/Steel-Foundation/SteelMC.git"


def git(*args, capture=False):
    """Run Git only in the build-owned source checkout."""
    return subprocess.run(
        ["git", "-C", str(SOURCE), *args], check=True,
        stdout=subprocess.PIPE if capture else None,
    ).stdout


def prepare():
    """Check out the pinned revision, refusing to discard local edits."""
    if not SOURCE.exists():
        SOURCE.mkdir()
        git("init", "--quiet")
        git("remote", "add", "origin", REMOTE)
    if git("status", "--porcelain", "--untracked-files=no", capture=True):
        raise RuntimeError("native/.steel has local edits; preserve them, then delete it to refetch")
    head = subprocess.run(["git", "-C", str(SOURCE), "rev-parse", "--verify", "--quiet", "HEAD"],
                          stdout=subprocess.PIPE).stdout.decode().strip()
    if head != REVISION:
        git("fetch", "--depth", "1", "origin", REVISION)
        git("checkout", "--quiet", "--detach", "FETCH_HEAD")


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
