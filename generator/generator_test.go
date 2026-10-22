package generator

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// testGenerator opens a real native generator and registers deterministic cleanup.
func testGenerator(t testing.TB, seed int64) *Generator {
	t.Helper()
	g, err := (Config{Workers: 4, CacheChunks: 512}).New(context.Background(), seed, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := g.Close(); err != nil {
			t.Error(err)
		}
	})
	world.DefaultBlockRegistry.Finalize()
	return g
}

// testChunk allocates a chunk with Dragonfly's actual overworld bounds and registry.
func testChunk() *chunk.Chunk { return chunk.New(world.DefaultBlockRegistry, cube.Range{-64, 319}) }

// chunkHash covers all block layers and biomes, including negative world heights.
func chunkHash(c *chunk.Chunk) [32]byte {
	h := sha256.New()
	var word [4]byte
	for y := int16(-64); y < 320; y++ {
		for z := uint8(0); z < 16; z++ {
			for x := uint8(0); x < 16; x++ {
				for _, v := range []uint32{c.Block(x, y, z, 0), c.Block(x, y, z, 1), c.Biome(x, y, z)} {
					binary.LittleEndian.PutUint32(word[:], v)
					h.Write(word[:])
				}
			}
		}
	}
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result
}

// TestTerrain checks varied terrain, native-resolution biomes, low fallback counts and concurrency.
func TestTerrain(t *testing.T) {
	g := testGenerator(t, 42)
	positions := []world.ChunkPos{{0, 0}, {1, 0}, {-1, 1}, {2, -1}}
	hashes := make([][32]byte, len(positions))
	var wg sync.WaitGroup
	var surfaces, trees atomic.Uint64
	for i, pos := range positions {
		wg.Go(func() {
			c := testChunk()
			if err := g.Generate(context.Background(), pos, c); err != nil {
				t.Error(err)
				return
			}
			hashes[i] = chunkHash(c)
			var solid, air, other int
			biomes := map[uint32]bool{}
			for y := int16(-64); y < 320; y++ {
				for z := uint8(0); z < 16; z++ {
					for x := uint8(0); x < 16; x++ {
						name, _, _ := c.BlockRegistry().RuntimeIDToState(c.Block(x, y, z, 0))
						if name == "minecraft:grass_block" {
							surfaces.Add(1)
						}
						if strings.HasSuffix(name, "_log") {
							trees.Add(1)
						}
						if name == "minecraft:seagrass" || name == "minecraft:kelp" || name == "minecraft:kelp_plant" {
							water, _, _ := c.BlockRegistry().RuntimeIDToState(c.Block(x, y, z, 1))
							if water != "minecraft:water" {
								t.Errorf("%s has no water layer", name)
							}
						}
						switch name {
						case "minecraft:air":
							air++
						case "minecraft:stone", "minecraft:deepslate":
							solid++
						default:
							other++
						}
						biomes[c.Biome(x, y, z)] = true
					}
				}
			}
			if air == 0 || solid == 0 || other < 256 {
				t.Errorf("%v: degenerate terrain: air=%d solid=%d other=%d", pos, air, solid, other)
			}
			if len(biomes) == 0 || (len(biomes) == 1 && biomes[0]) {
				t.Errorf("%v: biomes not set: %v", pos, biomes)
			}
			t.Logf("%v: air=%d stone/deepslate=%d other=%d biomes=%v", pos, air, solid, other, biomes)
		})
	}
	wg.Wait()
	if surfaces.Load() == 0 || trees.Load() == 0 {
		t.Errorf("missing surfaces or trees: grass=%d logs=%d", surfaces.Load(), trees.Load())
	}
	// Reopen another handle to the same seed, then check repeat calls and handle lifetime.
	second := testGenerator(t, 42)
	if err := g.Close(); err != nil {
		t.Fatal(err)
	}
	for i, pos := range positions {
		c := testChunk()
		if err := second.Generate(context.Background(), pos, c); err != nil {
			t.Fatal(err)
		}
		if got := chunkHash(c); got != hashes[i] {
			t.Errorf("%v changed on repeat generation", pos)
		}
	}
	stats := g.Stats()
	t.Logf("stats: %+v", stats)
	if stats.Chunks != 4 {
		t.Errorf("generated %d chunks, want 4", stats.Chunks)
	}
	if stats.UnmappedStates > 10 || stats.FallbackBlocks*200 > stats.Blocks || stats.UnmappedBiomes != 0 {
		t.Errorf("too many fallbacks: %+v", stats)
	}
	if err := g.Generate(context.Background(), world.ChunkPos{}, testChunk()); err == nil {
		t.Error("closed generator accepted work")
	}
}

// TestMappingFallback verifies exact states, waterlogging, per-block defaults and solid/air fallback.
func TestMappingFallback(t *testing.T) {
	g := testGenerator(t, 7)
	translator, err := g.translator(world.DefaultBlockRegistry)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"minecraft:stone", "minecraft:grass_block[snowy=false]", "minecraft:oak_log[axis=x]", "minecraft:water[level=0]"} {
		if _, ok := translator.lookup(key); !ok {
			t.Errorf("unmapped required state %s", key)
		}
	}
	leaves := translator.translate("minecraft:oak_leaves[distance=7,persistent=false,waterlogged=true]", "", false)
	if leaves.fallback || !leaves.waterlogged {
		t.Errorf("waterlogged leaves: %+v", leaves)
	}
	for _, tc := range []struct {
		key, def string
		solid    bool
		want     uint32
	}{
		{"minecraft:stone[unknown=true]", "minecraft:stone", true, translator.stone},
		{"example:solid", "", true, translator.stone},
		{"example:plant", "", false, translator.air},
	} {
		got := translator.translate(tc.key, tc.def, tc.solid)
		if got.id != tc.want || !got.fallback {
			t.Errorf("%s fallback: %+v", tc.key, got)
		}
		translator.translate(tc.key, tc.def, tc.solid)
	}
	if stats := g.Stats(); stats.UnmappedStates != 3 {
		t.Errorf("fallbacks should count each distinct state once: %+v", stats)
	}
}

// TestInvalidInputs exercises errors without writing a partial chunk.
func TestInvalidInputs(t *testing.T) {
	g := testGenerator(t, 8)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := g.Generate(ctx, world.ChunkPos{}, testChunk()); err == nil {
		t.Error("cancelled request accepted")
	}
	wrong := chunk.New(world.DefaultBlockRegistry, cube.Range{0, 255})
	if err := g.Generate(context.Background(), world.ChunkPos{}, wrong); err == nil {
		t.Error("non-overworld range accepted")
	}
	translator, err := g.translator(world.DefaultBlockRegistry)
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{nil, []byte("SWG1"), append([]byte("SWG1"), 0xc0, 0xff, 24, 0)} {
		if _, err := fillChunk(testChunk(), translator, data); err == nil {
			t.Error("truncated stream accepted")
		}
	}
}

// TestConfigRejectsInvalidLimits checks errors before any native context is allocated.
func TestConfigRejectsInvalidLimits(t *testing.T) {
	for _, config := range []Config{{Workers: -1}, {Workers: 257}, {CacheChunks: -1}, {CacheChunks: 4097}} {
		if g, err := config.New(context.Background(), 99, nil); err == nil {
			g.Close()
			t.Errorf("invalid config accepted: %+v", config)
		}
	}
	if DefaultWorkers() < 1 {
		t.Fatal("default worker count must be positive")
	}
}

// TestConcurrentSameChunk exercises duplicate queued requests and bounded-channel backpressure.
func TestConcurrentSameChunk(t *testing.T) {
	g, err := (Config{Workers: 4, CacheChunks: 8}).New(context.Background(), 33, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	world.DefaultBlockRegistry.Finalize()
	hashes := make([][32]byte, 32)
	var wg sync.WaitGroup
	for i := range hashes {
		wg.Go(func() {
			c := testChunk()
			if err := g.Generate(context.Background(), world.ChunkPos{3, 3}, c); err != nil {
				t.Error(err)
				return
			}
			hashes[i] = chunkHash(c)
		})
	}
	wg.Wait()
	for i, hash := range hashes {
		if hash != hashes[0] {
			t.Errorf("duplicate request %d returned different terrain", i)
		}
	}
	if stats := g.Stats(); stats.Chunks != 32 || stats.FallbackBlocks != 0 {
		t.Errorf("unexpected duplicate request stats: %+v", stats)
	}
}

// TestRegenerationAfterLongTraversal checks exact block output after eviction and region regeneration.
func TestRegenerationAfterLongTraversal(t *testing.T) {
	g, err := (Config{Workers: 8, CacheChunks: 8}).New(context.Background(), 42, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	world.DefaultBlockRegistry.Finalize()
	origin := testChunk()
	if err := g.Generate(context.Background(), world.ChunkPos{}, origin); err != nil {
		t.Fatal(err)
	}
	want := chunkHash(origin)
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for x := range jobs {
				if err := g.Generate(context.Background(), world.ChunkPos{int32(x), 0}, testChunk()); err != nil {
					t.Error(err)
				}
			}
		})
	}
	for x := 1; x <= 1100; x++ {
		jobs <- x
	}
	close(jobs)
	wg.Wait()
	again := testChunk()
	if err := g.Generate(context.Background(), world.ChunkPos{}, again); err != nil {
		t.Fatal(err)
	}
	if chunkHash(again) != want {
		t.Fatal("block layers or biomes changed after uncached regeneration")
	}
	if stats := g.Stats(); stats.Chunks != 1102 || stats.FallbackBlocks != 0 || stats.UnmappedBiomes != 0 {
		t.Errorf("unexpected traversal stats: %+v", stats)
	}
}

// TestDeterministicGenerationOrder compares native palettes and mapped chunks across fresh worlds, request orders and thread counts.
func TestDeterministicGenerationOrder(t *testing.T) {
	positions := []world.ChunkPos{{0, 0}, {127, 0}, {128, 0}, {-1, -1}, {0, 1}, {0, 0}}
	want := make(map[world.ChunkPos][2][32]byte)
	for run := range 2 {
		g, err := (Config{Workers: 2 + 6*run, CacheChunks: 1}).New(context.Background(), 42, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { g.Close() })
		world.DefaultBlockRegistry.Finalize()
		for i := range positions {
			index := i
			if run == 1 {
				index = len(positions) - 1 - i
			}
			pos := positions[index]
			c := testChunk()
			if err := g.Generate(context.Background(), pos, c); err != nil {
				t.Fatal(err)
			}
			data, err := g.native.generate(pos[0], pos[1])
			if err != nil {
				t.Fatal(err)
			}
			hash := [2][32]byte{sha256.Sum256(data), chunkHash(c)}
			if previous, exists := want[pos]; exists && previous != hash {
				t.Fatalf("%v changed in run %d at request %d", pos, run, i)
			}
			want[pos] = hash
		}
		if err := g.Close(); err != nil {
			t.Fatal(err)
		}
	}
}
