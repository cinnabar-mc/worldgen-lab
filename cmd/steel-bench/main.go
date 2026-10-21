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
	"time"

	"github.com/cinnabar-mc/worldgen-lab/generator"
	"github.com/df-mc/dragonfly/server/block/cube"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/df-mc/dragonfly/server/world/chunk"
)

// main measures a cold contiguous region with the requested number of Go chunk workers.
func main() {
	workers := flag.Int("workers", 1, "concurrent Go generation workers")
	count := flag.Int("chunks", 32, "number of unique chunks")
	seed := flag.Int64("seed", 42, "world seed")
	flag.Parse()
	if *workers < 1 || *count < 1 {
		slog.Error("workers and chunks must be positive")
		os.Exit(1)
	}
	gen, err := generator.New(context.Background(), *seed, slog.Default())
	if err != nil {
		slog.Error("open generator", "error", err)
		os.Exit(1)
	}
	world.DefaultBlockRegistry.Finalize()
	jobs := make(chan int)
	var wg sync.WaitGroup
	start := time.Now()
	for range *workers {
		wg.Go(func() {
			for i := range jobs {
				c := chunk.New(world.DefaultBlockRegistry, cube.Range{-64, 319})
				gen.GenerateChunk(world.ChunkPos{int32(i % 8), int32(i / 8)}, c)
			}
		})
	}
	for i := 0; i < *count; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	seconds := time.Since(start).Seconds()
	result := struct {
		Workers, Chunks          int
		Seconds, ChunksPerSecond float64
		Stats                    generator.Stats
	}{*workers, *count, seconds, float64(*count) / seconds, gen.Stats()}
	if err = json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
	if err = gen.Close(); err != nil {
		slog.Error("close generator", "error", err)
		os.Exit(1)
	}
}
