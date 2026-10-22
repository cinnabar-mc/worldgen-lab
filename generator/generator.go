// Package generator serves SteelMC's Java overworld terrain through Dragonfly.
package generator

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"

	_ "github.com/df-mc/dragonfly/server/block"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	_ "github.com/df-mc/dragonfly/server/world/biome"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// Generator is a concurrent, overworld-only Dragonfly generator. Call Close after the world stops.
type Generator struct {
	mu                                                       sync.RWMutex
	native                                                   *nativeContext
	table                                                    *mappingTable
	log                                                      *slog.Logger
	translators                                              sync.Map
	translatorMu                                             sync.Mutex
	chunks, blocks, fallbackBlocks, unmapped, unmappedBiomes atomic.Uint64
	spawnOnce                                                sync.Once
	spawn                                                    cube.Pos
}

// Stats reports cumulative generation and fallback counts for this Generator.
type Stats struct {
	Chunks         uint64 `json:"chunks"`
	Blocks         uint64 `json:"blocks"`
	FallbackBlocks uint64 `json:"fallback_blocks"`
	UnmappedStates uint64 `json:"unmapped_states"`
	UnmappedBiomes uint64 `json:"unmapped_biomes"`
}

// Config bounds native generation and retained encoded outputs for each seed.
// Use the same Workers value for Dragonfly's ChunkLoadWorkers.
type Config struct {
	// Workers is the Rayon thread count. Zero uses available cores minus one (minimum one).
	Workers int
	// CacheChunks bounds encoded outputs. Zero uses 256; two prefetched regions are retained separately.
	CacheChunks int
}

// DefaultWorkers leaves one available core for the server and client, with a minimum of one.
func DefaultWorkers() int { return max(1, runtime.NumCPU()-1) }

// New opens a generator with the default native worker and cache limits.
func New(ctx context.Context, seed int64, log *slog.Logger) (*Generator, error) {
	return (Config{}).New(ctx, seed, log)
}

// New opens a shared per-seed context, rejecting conflicting settings on another live handle.
func (config Config) New(ctx context.Context, seed int64, log *slog.Logger) (*Generator, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if config.Workers == 0 {
		config.Workers = DefaultWorkers()
	}
	if config.CacheChunks == 0 {
		config.CacheChunks = 256
	}
	if config.Workers < 1 || config.Workers > 256 {
		return nil, errors.New("workers must be 1..256")
	}
	if config.CacheChunks < 1 || config.CacheChunks > 4096 {
		return nil, errors.New("cache chunks must be 1..4096")
	}
	if log == nil {
		log = slog.Default()
	}
	table, err := loadTable()
	if err != nil {
		return nil, fmt.Errorf("load mappings: %w", err)
	}
	native, err := openNative(seed, config.Workers, config.CacheChunks)
	if err != nil {
		return nil, fmt.Errorf("open Steel context: %w", err)
	}
	return &Generator{native: native, table: table, log: log}, nil
}

// Close waits for in-flight calls, then releases this generator's native handle. It is idempotent.
func (g *Generator) Close() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.native != nil {
		g.native.close()
		g.native = nil
	}
	return nil
}

// Stats returns a snapshot. During generation, separate counters may advance between reads.
func (g *Generator) Stats() Stats {
	return Stats{g.chunks.Load(), g.blocks.Load(), g.fallbackBlocks.Load(), g.unmapped.Load(), g.unmappedBiomes.Load()}
}

// GenerateChunk fills a Dragonfly chunk. A native failure is fatal rather than persisting an empty chunk.
func (g *Generator) GenerateChunk(pos world.ChunkPos, c *chunk.Chunk) {
	if err := g.Generate(context.Background(), pos, c); err != nil {
		panic(fmt.Errorf("generate Steel chunk %v: %w", pos, err))
	}
}

// Generate fills one overworld chunk and returns errors to tools that can handle them.
func (g *Generator) Generate(ctx context.Context, pos world.ChunkPos, c *chunk.Chunk) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if c.Range() != (cube.Range{-64, 319}) {
		return errors.New("Steel generation requires range -64..319")
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.native == nil {
		return errors.New("Steel generator is closed")
	}
	translator, err := g.translator(c.BlockRegistry())
	if err != nil {
		return err
	}
	data, err := g.native.generate(pos[0], pos[1])
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		return err
	}
	fallback, err := fillChunk(c, translator, data)
	if err != nil {
		return err
	}
	g.chunks.Add(1)
	g.blocks.Add(16 * 16 * 384)
	g.fallbackBlocks.Add(fallback)
	return nil
}

// translator shares a typed mapping cache across chunks with the same finalized registry.
func (g *Generator) translator(registry chunk.BlockRegistry) (*translator, error) {
	if t, ok := g.translators.Load(registry); ok {
		return t.(*translator), nil
	}
	g.translatorMu.Lock()
	defer g.translatorMu.Unlock()
	if t, ok := g.translators.Load(registry); ok {
		return t.(*translator), nil
	}
	t, err := newTranslator(registry, g.table, g.log, &g.unmapped, &g.unmappedBiomes)
	if err == nil {
		g.translators.Store(registry, t)
	}
	return t, err
}

// DefaultSpawn finds the top of the generated origin column, including oceans.
func (g *Generator) DefaultSpawn(world.Dimension) cube.Pos {
	g.spawnOnce.Do(func() {
		world.DefaultBlockRegistry.Finalize()
		c := chunk.New(world.DefaultBlockRegistry, cube.Range{-64, 319})
		g.GenerateChunk(world.ChunkPos{}, c)
		y := int16(317)
		for y > -64 && c.Block(8, y, 8, 0) == c.BlockRegistry().AirRuntimeID() {
			y--
		}
		g.spawn = cube.Pos{8, int(y) + 2, 8}
	})
	return g.spawn
}

// wireReader reads the little-endian versioned native stream with bounds checks.
type wireReader struct {
	data   []byte
	offset int
	err    error
}

// take advances over exactly n bytes, recording truncated buffers as an error.
func (r *wireReader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > len(r.data)-r.offset {
		r.err = errors.New("truncated Steel chunk")
		return nil
	}
	v := r.data[r.offset : r.offset+n]
	r.offset += n
	return v
}

// u16 reads an unsigned little-endian value.
func (r *wireReader) u16() uint16 {
	v := r.take(2)
	if v == nil {
		return 0
	}
	return binary.LittleEndian.Uint16(v)
}

// text reads a length-prefixed Java state or biome name.
func (r *wireReader) text() string { return string(r.take(int(r.u16()))) }

// fillChunk translates each section palette once, then assigns its block and biome indices.
func fillChunk(c *chunk.Chunk, t *translator, data []byte) (uint64, error) {
	r := wireReader{data: data}
	if string(r.take(4)) != "SWG1" || int16(r.u16()) != -64 || r.u16() != 24 {
		return 0, errors.New("invalid Steel chunk header")
	}
	var fallback uint64
	for section := 0; section < 24; section++ {
		count := int(r.u16())
		if count < 1 || count > 4096 {
			return 0, errors.New("invalid Steel block palette")
		}
		palette := make([]translatedState, count)
		for i := range palette {
			state, def := r.text(), r.text()
			flags := r.take(1)
			if r.err != nil {
				return 0, r.err
			}
			palette[i] = t.translate(state, def, flags[0]&1 != 0)
			palette[i].waterlogged = palette[i].waterlogged || flags[0]&2 != 0
		}
		indices := r.take(8192)
		if r.err != nil {
			return 0, r.err
		}
		sub := chunk.NewSubChunk(t.air)
		if len(palette) == 1 {
			for i := 0; i < len(indices); i += 2 {
				if binary.LittleEndian.Uint16(indices[i:]) != 0 {
					return 0, errors.New("invalid Steel block index")
				}
			}
			state := palette[0]
			if state.id != t.air {
				sub.Layer(0).Palette().Replace(func(uint32) uint32 { return state.id })
			}
			if state.waterlogged {
				sub.Layer(1).Palette().Replace(func(uint32) uint32 { return t.water })
			}
			if state.fallback {
				fallback += 4096
			}
		} else {
			blocks := sub.Layer(0)
			var water *chunk.PalettedStorage
			for i := 0; i < 4096; i++ {
				index := int(binary.LittleEndian.Uint16(indices[i*2:]))
				if index >= len(palette) {
					return 0, errors.New("invalid Steel block index")
				}
				state := palette[index]
				x, y, z := uint8(i%16), uint8(i/256), uint8((i/16)%16)
				if state.id != t.air {
					blocks.Set(x, y, z, state.id)
				}
				if state.waterlogged {
					if water == nil {
						water = sub.Layer(1)
					}
					water.Set(x, y, z, t.water)
				}
				if state.fallback {
					fallback++
				}
			}
		}
		c.SetSubChunk(int16(section), sub)

		count = int(r.u16())
		if count < 1 || count > 64 {
			return 0, errors.New("invalid Steel biome palette")
		}
		biomes := make([]uint32, count)
		for i := range biomes {
			biomes[i] = t.biome(r.text())
		}
		for i := 0; i < 64; i++ {
			index := int(r.u16())
			if r.err != nil {
				return 0, r.err
			}
			if index >= len(biomes) {
				return 0, errors.New("invalid Steel biome index")
			}
			// Dragonfly stores biomes at block resolution, so expand every quart cell.
			for dy := 0; dy < 4; dy++ {
				for dz := 0; dz < 4; dz++ {
					for dx := 0; dx < 4; dx++ {
						c.SetBiome(uint8((i%4)*4+dx), int16(-64+section*16+(i/16)*4+dy), uint8(((i/4)%4)*4+dz), biomes[index])
					}
				}
			}
		}
	}
	if r.err != nil {
		return 0, r.err
	}
	if r.offset != len(data) {
		return 0, errors.New("trailing Steel chunk bytes")
	}
	return fallback, nil
}
