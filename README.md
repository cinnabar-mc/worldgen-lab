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

The first Cargo build fetches SteelMC and its dependencies. SteelMC's build scripts also download the Java server data they need. Those inputs stay in Cargo's dependency cache; do not redistribute them. The Go binaries statically link the Rust bridge and need no Rust installation at runtime. Build the Rust library before running Go builds or tests. After changing Rust, add `-a` to the Go build or test command: Go's cache does not track external static archive changes. The default Cargo target directory is required by the cgo linker flags.

## Run

```sh
./bin/steel-server -addr 127.0.0.1:19132 -dir worlds/steel -seed 42
```

Join `127.0.0.1:19132` with a compatible Bedrock client. Xbox Live authentication is enabled. For account-free local tests, add `-offline` and bind to a loopback IP; the server rejects that option on other addresses. For a client on another machine, bind to the appropriate network address with `-addr`. New players start in creative mode. `-workers` controls both Dragonfly's chunk workers and the native Rayon pool. Its default is the available core count minus one, with a minimum of one. Ctrl-C closes the server and saves the world and players.

Each data directory records its seed and generator revision. Choose a new `-dir` when changing either; existing chunks must not be mixed with a different generator.

## Use the generator

```go
workers := generator.DefaultWorkers()
gen, err := (generator.Config{Workers: workers}).New(context.Background(), 42, slog.Default())
if err != nil {
    return err
}
defer gen.Close() // after the Dragonfly worlds have stopped
conf.ChunkLoadWorkers = workers
conf.Generator = func(dim world.Dimension) world.Generator {
    if dim == world.Overworld {
        return gen
    }
    return world.NopGenerator{}
}
```

`Generator` supports concurrent `GenerateChunk` calls. A bounded per-seed host queue waits on completion channels. The host batches queued positions, pipelines their status futures, and returns each encoded chunk as soon as it completes; callers never sleep-poll or contend on a scheduler mutex. Rust shares one seed context across live generator handles, and Go keeps mapping caches per finalized Dragonfly block registry. Each returned section contains a Java palette keyed by block name and sorted properties, 4096 indices, and 64 biome samples. Java numeric IDs never cross the bridge.

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
./bin/steel-bench -workers 1 -chunks 4096 -seed 42
./bin/steel-bench -workers 4 -chunks 4096 -seed 42
./bin/steel-bench -workers 8 -chunks 4096 -seed 42
# Replace 12 with the available core count on your machine.
./bin/steel-bench -workers 12 -chunks 4096 -seed 42
./bin/steel-bench -workers 8 -pattern line -chunks 5000 -progress 500
```

Run each benchmark command in a fresh process. It generates a cold, unique contiguous region and includes native scheduling, mapping and chunk filling. It excludes context initialization. The same `-workers` value controls Go chunk workers and native Rayon threads. Use a sufficiently large region (for example `-chunks 4096`) to measure sustained throughput. `PeakRSSBytes` includes Go and native memory. Results are JSON with elapsed time, chunks per second and fallback counts. Use `-cache-chunks` to compare bounded cache capacities; a limit smaller than the active dependency halo can force regeneration.

## Dependencies and mapping regeneration

SteelMC is pinned in `native/Cargo.toml` and `native/Cargo.lock` to `ff8e48c5b751788a6939be57b32932fbbc85ecc3` in the cinnabar-mc/SteelMC fork (0.15.5, Java 26.2). The bridge uses `steel-worldgen` through SteelMC's `steel-core` driver, which owns the surface, carver, feature and structure stages. It advances the upstream dependency pyramid through Full without running gameplay ticks. The fork exposes scheduling and status-holder APIs for embedders and adds an optional bound on saved RAM snapshots; generation algorithms are unchanged.

GeyserMC mappings are pinned to `0fd435d3d4617dd458c6ee0e6889a5245483bf5f` (Java 26.2 → Bedrock 1.26.50). The readable Java state keys come from mappings-generator release `2.0.1-build.1`. The generator verifies the archive checksum and requires its block and biome mappings to exactly equal the pinned repository files. It then builds the table by Java names and properties, never by numeric registry IDs.

```sh
python3 scripts/generate_mappings.py
```

The generated compressed table is committed; normal builds need no Python or mapping download. GeyserMC's MIT notice is in [generator/mappings/LICENSE](generator/mappings/LICENSE). SteelMC is AGPL-3.0; this project uses the same license, included in [LICENSE](LICENSE). GeyserMC licenses its contributors' original work and does not relicense Minecraft or Microsoft/Mojang data.

## Limits

Only the overworld range −64 ≤ Y < 320 is supported. SteelMC's upstream structure placement and biome decorations run, but Java block entities, inventories, loot and entities are not translated into Dragonfly. This is terrain generation, not Java server gameplay parity. Other dimensions are empty in the standalone server.

Uncached regeneration is not guaranteed to reproduce every decoration block. Persist served chunks through Dragonfly.

Each context retains at most `Config.CacheChunks` encoded outputs and the same number of saved native snapshots (default 2048, configurable from 1 to 4096). The ingress queue and in-flight futures are each capped at twice the worker count. Generation uses Steel's dependency halo; once a requested chunk is encoded, its tickets and holder references are released so intermediates can unload when no pending neighbor needs them. After 1024 completions, the host drains pending generation and replaces its disposable world, bounding auxiliary world indexes too. The seeded generator and bounded output cache survive that reset. Dragonfly remains the persistent owner of served chunks. Close all handles when finished.
