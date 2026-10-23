# worldgen-lab

Java Edition 26.3 overworld terrain from [SteelMC](https://github.com/Steel-Foundation/SteelMC), served to Bedrock 1.26.50 clients through [Dragonfly](https://github.com/df-mc/dragonfly).

## Build

Requires Go 1.26, rustup, Python 3, Git and a C compiler.

```sh
python3 native/build.py   # fetches pinned SteelMC, applies native/patches, builds the Rust bridge
go build -o bin/ ./cmd/...
```

Rebuild Go with `-a` after changing Rust; Go's cache does not track the static library.

## Run

```sh
./bin/steel-server -seed 42                 # listens on 127.0.0.1:19132
./bin/steel-server -seed 42 -offline        # loopback only, no Xbox sign-in
```

Use a new `-dir` when changing the seed or generator revision. `-workers` sets both Dragonfly and native generation threads. `-radius` caps the client view distance (default 12).

Testing commands (available to every player):

- `/fly` toggles permission to fly in survival or adventure; double-tap jump to start flying.
- `/speed 4` scales walking and direct flight to 4 times their default speeds. While flying,
  movement keys steer horizontally, jump rises, sneak descends, and releasing the controls stops
  motion. The command reports the flight rate in blocks/s. `/speed reset`, `/speed 1`, or `/speed`
  restores normal walking and ability flight. Speed settings reset on reconnect.
- `/time set day` changes the time; named choices are day, noon, sunset, night, midnight and sunrise.
  `/time set 6000` sets a tick count, and `/time add 1000` advances time by that many ticks.
  These commands preserve the current daylight cycle setting.

Use `/speed` to move through fresh terrain at a repeatable speed when testing chunk generation.

## Use as a Dragonfly generator

```go
gen, err := generator.Config{Workers: generator.DefaultWorkers()}.New(ctx, seed, slog.Default())
if err != nil {
    return err
}
defer gen.Close() // after the worlds stop
conf.Generator = func(dim world.Dimension) world.Generator {
    if dim == world.Overworld {
        return gen
    }
    return world.NopGenerator{}
}
```

Chunks stream from one long-lived SteelMC world, so each request reuses the generation work done for its neighbours, as on a Java server. As in Java, neighbouring features are placed in completion order, so a chunk can differ slightly between runs; a world never changes a chunk it has served. Once the dependency halos of a world's served chunks cover 16,384 chunk positions, a fresh world prepared with the latest chunks takes over to bound memory; a radius-12 view flown in a straight line reaches this after roughly 8,000 chunks. Set `Deterministic` to generate fixed regions in disposable worlds instead: output then never depends on request order, but the first chunk of each region takes seconds.

Java block states map to Bedrock through pinned GeyserMC data; unmapped states fall back to stone or air and are counted in `gen.Stats()`.

## Benchmark

```sh
./bin/steel-bench -workers 8 -pattern grid -chunks 4096
./bin/steel-bench -pattern player -radius 12 -speed 33   # elytra; 5.6 is sprinting
```

The `player` pattern simulates a player's view disc moving through fresh terrain, with closest-first requests at 32 per tick like `steel-server`. It reports time to the first chunk, time to fill the initial view, p50 and p95 latency from when a chunk enters view, and sustained chunks per second.

## Limits

Overworld only (Y −64 to 319). No block entities, loot or mobs.

## Licence

AGPL-3.0, following SteelMC. GeyserMC mapping data is MIT ([notice](generator/mappings/LICENSE)). Regenerate it with `scripts/generate_mappings.py`.
