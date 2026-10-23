package generator

import (
	"encoding/binary"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// biomeSection describes one section of a prepared native bridge stream.
type biomeSection struct {
	palette []string
	indices [64]uint16
}

// conversionTranslator prepares the real mapping table without starting native generation.
func conversionTranslator(t testing.TB) *translator {
	t.Helper()
	world.DefaultBlockRegistry.Finalize()
	table, err := loadTable()
	if err != nil {
		t.Fatal(err)
	}
	tr, err := newTranslator(world.DefaultBlockRegistry, table, slog.New(slog.NewTextHandler(io.Discard, nil)), new(atomic.Uint64), new(atomic.Uint64))
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

// biomeColumnData encodes air blocks and the supplied biome sections, returning biome index offsets.
func biomeColumnData(sections [24]biomeSection) ([]byte, [24]int) {
	data := append([]byte("SWG1"), 0xc0, 0xff, 24, 0)
	var offsets [24]int
	for section, biomes := range sections {
		data = binary.LittleEndian.AppendUint16(data, 1)
		data = binary.LittleEndian.AppendUint16(data, uint16(len("minecraft:air")))
		data = append(data, "minecraft:air"...)
		data = append(data, 0, 0, 0) // Empty default state and no flags.
		data = append(data, make([]byte, 8192)...)
		data = binary.LittleEndian.AppendUint16(data, uint16(len(biomes.palette)))
		for _, name := range biomes.palette {
			data = binary.LittleEndian.AppendUint16(data, uint16(len(name)))
			data = append(data, name...)
		}
		offsets[section] = len(data)
		for _, index := range biomes.indices {
			data = binary.LittleEndian.AppendUint16(data, index)
		}
	}
	return data, offsets
}

// conversionSections covers uniform, duplicate, zero and mixed translated biome values.
func conversionSections(kind string) [24]biomeSection {
	var sections [24]biomeSection
	for section := range sections {
		palette := []string{"minecraft:plains"}
		if kind == "zero" {
			palette = []string{"minecraft:ocean"}
		} else if kind == "duplicate" || kind == "selected" || kind == "mixed" {
			palette = []string{"minecraft:plains", "minecraft:plains", "minecraft:desert", "minecraft:ocean"}
		}
		sections[section].palette = palette
		for cell := range sections[section].indices {
			switch kind {
			case "selected":
				sections[section].indices[cell] = 2
			case "duplicate":
				sections[section].indices[cell] = uint16(cell % 2)
			case "mixed":
				sections[section].indices[cell] = uint16((section + cell) % 4)
			}
		}
	}
	return sections
}

// TestFillChunkBiomes checks every block position, including all negative-height sections.
func TestFillChunkBiomes(t *testing.T) {
	tr := conversionTranslator(t)
	for _, kind := range []string{"uniform", "zero", "duplicate", "selected", "mixed"} {
		t.Run(kind, func(t *testing.T) {
			sections := conversionSections(kind)
			data, _ := biomeColumnData(sections)
			c := testChunk()
			// Conversion must also replace existing biome values, including when the new ID is zero.
			c.SetBiome(0, -64, 0, 99)
			fallback, err := fillChunk(c, tr, data)
			if err != nil || fallback != 0 {
				t.Fatalf("fillChunk: fallback=%d, error=%v", fallback, err)
			}
			for y := -64; y < 320; y++ {
				for z := 0; z < 16; z++ {
					for x := 0; x < 16; x++ {
						section := sections[(y+64)/16]
						cell := ((y+64)%16/4)*16 + (z/4)*4 + x/4
						want := tr.table.Biomes[section.palette[section.indices[cell]]]
						if got := c.Biome(uint8(x), int16(y), uint8(z)); got != want {
							t.Fatalf("(%d,%d,%d): got %d, want %d", x, y, z, got, want)
						}
					}
				}
			}
			c.SetBiome(0, -64, 0, 99)
			if got := c.Biome(1, -64, 0); got != tr.table.Biomes[sections[0].palette[sections[0].indices[0]]] {
				t.Fatalf("mutation changed another block: got %d", got)
			}
			if got := c.Biome(0, -48, 0); got != tr.table.Biomes[sections[1].palette[sections[1].indices[0]]] {
				t.Fatalf("mutation changed another section: got %d", got)
			}
		})
	}
}

// TestFillChunkInvalidBiomeIndices checks early and late errors before a uniform section can be installed.
func TestFillChunkInvalidBiomeIndices(t *testing.T) {
	tr := conversionTranslator(t)
	for _, kind := range []string{"uniform", "zero", "duplicate", "mixed"} {
		for _, section := range []int{0, 23} {
			for _, cell := range []int{0, 63} {
				sections := conversionSections(kind)
				data, offsets := biomeColumnData(sections)
				binary.LittleEndian.PutUint16(data[offsets[section]+cell*2:], uint16(len(sections[section].palette)))
				if _, err := fillChunk(testChunk(), tr, data); err == nil || !strings.Contains(err.Error(), "invalid Steel biome index") {
					t.Fatalf("%s section=%d cell=%d: got %v", kind, section, cell, err)
				}
			}
		}
	}
}

// TestFillChunkTruncatedBiomeIndices checks that a short index stream cannot take the uniform path.
func TestFillChunkTruncatedBiomeIndices(t *testing.T) {
	tr := conversionTranslator(t)
	data, offsets := biomeColumnData(conversionSections("duplicate"))
	for _, section := range []int{0, 23} {
		for _, length := range []int{0, 1, 2, 126, 127} {
			if _, err := fillChunk(testChunk(), tr, data[:offsets[section]+length]); err == nil || !strings.Contains(err.Error(), "truncated Steel chunk") {
				t.Fatalf("section=%d bytes=%d: got %v", section, length, err)
			}
		}
	}
}

// BenchmarkFillChunk measures complete Go conversion of prepared columns, excluding native generation.
func BenchmarkFillChunk(b *testing.B) {
	tr := conversionTranslator(b)
	for _, kind := range []string{"uniform", "zero", "duplicate", "mixed"} {
		b.Run(kind, func(b *testing.B) {
			data, _ := biomeColumnData(conversionSections(kind))
			benchmarkFillChunk(b, tr, data)
		})
	}
}

// BenchmarkFillGeneratedChunk converts a prepared native column; native generation is outside the timer.
func BenchmarkFillGeneratedChunk(b *testing.B) {
	g := testGenerator(b, 42)
	tr, err := g.translator(world.DefaultBlockRegistry)
	if err != nil {
		b.Fatal(err)
	}
	data, err := g.native.generate(0, 0)
	if err != nil {
		b.Fatal(err)
	}
	benchmarkFillChunk(b, tr, data)
}

// benchmarkFillChunk measures fresh-column allocation and conversion using already prepared bytes.
func benchmarkFillChunk(b *testing.B, tr *translator, data []byte) {
	var c *chunk.Chunk
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		c = testChunk()
		if _, err := fillChunk(c, tr, data); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(len(chunk.EncodeBiomes(c, chunk.NetworkEncoding))), "biome-B/column")
}
