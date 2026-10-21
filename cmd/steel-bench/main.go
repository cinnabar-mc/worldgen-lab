// steel-bench measures uncached generation through the complete cgo and Dragonfly path.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cinnabar-mc/worldgen-lab/generator"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// main measures a cold contiguous region with the requested number of Go chunk workers.
func main() {
	workers := flag.Int("workers", generator.DefaultWorkers(), "Go chunk workers and native Rayon threads")
	cache := flag.Int("cache-chunks", 0, "retained output and snapshot limit (zero uses generator default)")
	count := flag.Int("chunks", 32, "number of unique chunks")
	pattern := flag.String("pattern", "grid", "chunk layout: grid or line")
	progress := flag.Int("progress", 0, "report RSS and progress every this many chunks (zero disables)")
	seed := flag.Int64("seed", 42, "world seed")
	flag.Parse()
	if *workers < 1 || *count < 1 || (*pattern != "grid" && *pattern != "line") || *progress < 0 {
		slog.Error("invalid workers, chunks, pattern or progress arguments")
		os.Exit(1)
	}
	gen, err := (generator.Config{Workers: *workers, CacheChunks: *cache}).New(context.Background(), *seed, slog.Default())
	if err != nil {
		slog.Error("open generator", "error", err)
		os.Exit(1)
	}
	world.DefaultBlockRegistry.Finalize()
	jobs := make(chan int)
	var wg sync.WaitGroup
	var completed atomic.Uint64
	var output sync.Mutex
	start := time.Now()
	for range *workers {
		wg.Go(func() {
			for i := range jobs {
				c := chunk.New(world.DefaultBlockRegistry, cube.Range{-64, 319})
				pos := world.ChunkPos{int32(i % 64), int32(i / 64)}
				if *pattern == "line" {
					pos = world.ChunkPos{int32(i), 0}
				}
				gen.GenerateChunk(pos, c)
				done := completed.Add(1)
				if *progress > 0 && done%uint64(*progress) == 0 {
					output.Lock()
					_ = json.NewEncoder(os.Stderr).Encode(struct {
						Chunks       uint64
						Seconds      float64
						PeakRSSBytes uint64
					}{done, time.Since(start).Seconds(), peakRSS()})
					output.Unlock()
				}
			}
		})
	}
	for i := 0; i < *count; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	seconds := time.Since(start).Seconds()

	if err = gen.Close(); err != nil {
		slog.Error("close generator", "error", err)
		os.Exit(1)
	}
	result := struct {
		Workers, Chunks          int
		Seconds, ChunksPerSecond float64
		PeakRSSBytes             uint64
		Stats                    generator.Stats
	}{*workers, *count, seconds, float64(*count) / seconds, peakRSS(), gen.Stats()}
	if err = json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}
