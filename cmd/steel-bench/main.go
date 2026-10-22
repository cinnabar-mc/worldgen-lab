// steel-bench measures uncached generation through the complete cgo and Dragonfly path.
// The player pattern also reports per-chunk latency for a moving view disc.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"slices"
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
	cache := flag.Int("cache-chunks", 0, "served-output cache limit (zero uses generator default)")
	count := flag.Int("chunks", 30000, "number of unique chunks")
	pattern := flag.String("pattern", "line", "chunk layout: grid, line or player")
	radius := flag.Int("radius", 8, "player view radius in chunks")
	speed := flag.Float64("speed", 5.6, "player speed in blocks/s (sprint 5.6, elytra 33)")
	duration := flag.Float64("seconds", 30, "player movement duration")
	budget := flag.Int("budget", 32, "player chunk requests per 50 ms tick")
	loadWorkers := flag.Int("load-workers", 0, "player blocking chunk workers (zero uses -workers)")
	progress := flag.Int("progress", 0, "report RSS and progress every this many chunks (zero disables)")
	seed := flag.Int64("seed", 42, "world seed")
	flag.Parse()
	if *loadWorkers == 0 {
		*loadWorkers = *workers
	}
	if *workers < 1 || *count < 1 || !slices.Contains([]string{"grid", "line", "player"}, *pattern) || *progress < 0 ||
		*radius < 1 || *speed < 0 || *duration <= 0 || *budget < 1 || *loadWorkers < 1 {
		slog.Error("invalid arguments")
		os.Exit(1)
	}
	gen, err := (generator.Config{Workers: *workers, CacheChunks: *cache}).New(context.Background(), *seed, slog.Default())
	if err != nil {
		slog.Error("open generator", "error", err)
		os.Exit(1)
	}
	world.DefaultBlockRegistry.Finalize()
	if *pattern == "player" {
		opts := playerOptions{*radius, *speed, time.Duration(*duration * float64(time.Second)), *budget, *loadWorkers}
		result := simulatePlayer(opts, func(pos world.ChunkPos) {
			gen.GenerateChunk(pos, chunk.New(world.DefaultBlockRegistry, cube.Range{-64, 319}))
		})
		if err = gen.Close(); err != nil {
			slog.Error("close generator", "error", err)
			os.Exit(1)
		}
		_ = json.NewEncoder(os.Stdout).Encode(struct {
			playerResult
			Workers      int
			PeakRSSBytes uint64
		}{result, *workers, peakRSS()})
		return
	}
	jobs := make(chan int)
	var wg sync.WaitGroup
	var completed atomic.Uint64
	var output sync.Mutex
	start := time.Now()
	var reported uint64
	var reportedSeconds float64
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
					if done <= reported {
						output.Unlock()
						continue
					}
					elapsed := time.Since(start).Seconds()
					intervalChunks := done - reported
					intervalSeconds := elapsed - reportedSeconds
					_ = json.NewEncoder(os.Stderr).Encode(struct {
						Chunks, IntervalChunks                            uint64
						Seconds, IntervalSeconds, IntervalChunksPerSecond float64
						PeakRSSBytes                                      uint64
					}{done, intervalChunks, elapsed, intervalSeconds, float64(intervalChunks) / intervalSeconds, peakRSS()})
					reported, reportedSeconds = done, elapsed
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
		Pattern                  string
		Stats                    generator.Stats
	}{*workers, *count, seconds, float64(*count) / seconds, peakRSS(), *pattern, gen.Stats()}
	if err = json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fmt.Fprintln(os.Stderr, err)
	}
}
