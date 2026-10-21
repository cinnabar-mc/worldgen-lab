#!/usr/bin/env python3
"""Generate name/property mappings from a pinned Geyser release and source commit."""

import argparse
import gzip
import hashlib
import io
import json
from pathlib import Path
import urllib.request
import zipfile

COMMIT = "0fd435d3d4617dd458c6ee0e6889a5245483bf5f"
RELEASE = "2.0.1-build.1"
ARCHIVE_SHA256 = "c05dd27ab743446e764ca2530c826eb32424378ee3d34e279a057633ac2dfd5a"
URL = f"https://github.com/GeyserMC/mappings-generator/releases/download/{RELEASE}/mappings-v2.0.1-full.zip"


def fetch(url):
    """Read one version-pinned upstream resource."""
    with urllib.request.urlopen(url, timeout=120) as response:
        return response.read()


def canonical(state):
    """Sort Java properties so matching never depends on their serialized order."""
    name, bracket, properties = state.partition("[")
    return name if not bracket else name + "[" + ",".join(sorted(properties[:-1].split(","))) + "]"


def generate(archive):
    """Validate the release against the mappings commit and return the translated table."""
    if hashlib.sha256(archive).hexdigest() != ARCHIVE_SHA256:
        raise ValueError("Geyser archive checksum mismatch")
    with zipfile.ZipFile(io.BytesIO(archive)) as source:
        # The release supplies the readable Java keys omitted from blocks.nbt.
        # Equality ties those keys to the pinned mappings without pairing registry IDs.
        for name in ("blocks.nbt", "biomes.json"):
            original = fetch(f"https://raw.githubusercontent.com/GeyserMC/mappings/{COMMIT}/{name}")
            if source.read("mappings/" + name) != original:
                raise ValueError(f"Geyser release does not match pinned {name}")
        blocks = {}
        for java, entry in json.loads(source.read("data/blocks_debug.json")).items():
            name = entry.get("bedrock_identifier", java.split("[")[0])
            if ":" not in name:
                name = "minecraft:" + name
            blocks[canonical(java)] = {"name": name, "states": entry.get("state", {})}
        biomes = {name: value["bedrock_id"] for name, value in json.loads(source.read("mappings/biomes.json")).items()}
    return {"blocks": blocks, "biomes": biomes}


def main():
    """Write a deterministic compressed table, optionally using a previously downloaded archive."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--archive", type=Path)
    parser.add_argument("--output", type=Path, default=Path("generator/mappings/table.json.gz"))
    args = parser.parse_args()
    table = generate(args.archive.read_bytes() if args.archive else fetch(URL))
    encoded = json.dumps(table, sort_keys=True, separators=(",", ":")).encode()
    args.output.parent.mkdir(parents=True, exist_ok=True)
    args.output.write_bytes(gzip.compress(encoded, mtime=0))
    print(f"Wrote {len(table['blocks'])} block states and {len(table['biomes'])} biomes")


if __name__ == "__main__":
    main()
