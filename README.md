# worldgen-lab

SteelMC's Java Edition 26.2 overworld generation served to Bedrock 1.26.50 clients through Dragonfly. This is an AGPL-3.0 experiment, kept separate from Cinnabar.

## Build

Install Go 1.26, Rust through rustup, and a C compiler (Xcode command line tools on macOS). The pinned Rust toolchain is selected by `rust-toolchain.toml`.

```sh
cargo build --locked --release --manifest-path native/Cargo.toml
mkdir -p bin
go build -o bin/steel-server ./cmd/steel-server
go build -o bin/steel-bench ./cmd/steel-bench
```

The first Cargo build fetches SteelMC and its dependencies. SteelMC's build scripts also download the Java server data they need. Those inputs stay in Cargo's dependency cache; do not redistribute them. The Go binaries statically link the Rust bridge and need no Rust installation at runtime. Build the Rust library before running Go builds or tests. The default Cargo target directory is required by the cgo linker flags.

## Run

```sh
./bin/steel-server -addr 127.0.0.1:19132 -dir worlds/steel -seed 42
```

Join `127.0.0.1:19132` with a compatible Bedrock client. Xbox Live authentication is enabled. For a client on another machine, bind to the appropriate network address with `-addr`. New players start in creative mode. `-workers` controls Dragonfly's chunk workers (default 4). Ctrl-C closes the server and saves the world and players.

Each data directory records its seed and generator revision. Choose a new `-dir` when changing either; existing chunks must not be mixed with a different generator.

## Use the generator

```go
gen, err := generator.New(context.Background(), 42, slog.Default())
if err != nil {
    return err
}
defer gen.Close() // after the Dragonfly worlds have stopped
conf.Generator = func(dim world.Dimension) world.Generator {
    if dim == world.Overworld {
        return gen
    }
    return world.NopGenerator{}
}
```

`Generator` supports concurrent `GenerateChunk` calls. Rust shares one seed context across live generator handles, and Go keeps mapping caches per finalized Dragonfly block registry. Each returned section contains a Java palette keyed by block name and sorted properties, 4096 indices, and 64 biome samples. Java numeric IDs never cross the bridge.

Exact Java states use Geyser mappings. A missing state tries its block's actual Java default state; if that also fails, solid states become stone and other states become air. Each distinct fallback is counted and logged once per target registry. `gen.Stats()` also reports affected block occurrences and missing biome names. Waterlogged Java states receive water in Bedrock's second block layer.

## Cinnabar integration

The standalone server can be joined directly from Cinnabar's multiplayer screen. Cinnabar's importable `tools/localserver` generator registration API is not available in the pinned integration yet, so this repository does not ship a `cmd/localserver` wrapper.

Once that API lands, the wrapper will import its package and register `steel` with the label `SteelMC terrain`. Cinnabar's development build will then be able to launch that wrapper with:

```sh
CINNABAR_LOCAL_SERVER=/absolute/path/to/localserver <cinnabar-dev-command>
```

The standalone `steel-server` does not implement Cinnabar's local-world process protocol and cannot be used as that override.

## Verify and measure

```sh
go test -count=1 -v ./...
go test -race -count=1 ./...
./bin/steel-bench -workers 1 -chunks 32 -seed 42
./bin/steel-bench -workers 4 -chunks 32 -seed 42
```

Run each benchmark command in a fresh process. It generates a cold, unique contiguous region and includes native scheduling, mapping and chunk filling. It excludes context initialization. SteelMC's internal Rayon pool has four threads in both runs, so `-workers` measures the effect of concurrent Dragonfly requests. Results are JSON with elapsed time, chunks per second and fallback counts.

## Dependencies and mapping regeneration

SteelMC is pinned in `native/Cargo.toml` and `native/Cargo.lock` to `6749ecf6d24a420d61a997779219edec0b21da9e` (0.15.5, Java 26.2). The bridge uses `steel-worldgen` through SteelMC's `steel-core` driver, which owns the surface, carver, feature and structure stages. It advances the upstream dependency pyramid through Full without running gameplay ticks. The upstream `test-framework` feature exposes the scheduling-only entry point needed by an embedder.

GeyserMC mappings are pinned to `0fd435d3d4617dd458c6ee0e6889a5245483bf5f` (Java 26.2 → Bedrock 1.26.50). The readable Java state keys come from mappings-generator release `2.0.1-build.1`. The generator verifies the archive checksum and requires its block and biome mappings to exactly equal the pinned repository files. It then builds the table by Java names and properties, never by numeric registry IDs.

```sh
python3 scripts/generate_mappings.py
```

The generated compressed table is committed; normal builds need no Python or mapping download. GeyserMC's MIT notice is in [generator/mappings/LICENSE](generator/mappings/LICENSE). SteelMC is AGPL-3.0; this project uses the same license, included in [LICENSE](LICENSE). GeyserMC licenses its contributors' original work and does not relicense Minecraft or Microsoft/Mojang data.

## Limits

Only the overworld range −64 ≤ Y < 320 is supported. SteelMC's upstream structure placement and biome decorations run, but Java block entities, inventories, loot and entities are not translated into Dragonfly. This is terrain generation, not Java server gameplay parity. Other dimensions are empty in the standalone server.

SteelMC retains in-memory generated data for the life of a seed context. Close all handles when finished; long exploration sessions may consume substantial memory. The experiment currently favors reusing the complete upstream generation driver over a separate bounded cache.
