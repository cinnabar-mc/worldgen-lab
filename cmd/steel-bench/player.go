package main

import (
	"math"
	"slices"
	"sync"
	"time"

	"github.com/df-mc/dragonfly/server/world"
)

// playerOptions mirrors Dragonfly's loader: a closest-first queue, a per-tick
// request budget and a FIFO pool of blocking chunk workers.
type playerOptions struct {
	Radius      int
	Speed       float64 // blocks per second along +X
	Duration    time.Duration
	Budget      int // chunk requests issued per 50 ms tick
	LoadWorkers int
}

// playerResult reports latency from the moment a chunk enters the view disc.
// JoinMs is null unless every chunk of the initial disc was served; UnservedAtEnd counts
// chunks of the final disc not served when movement stopped.
type playerResult struct {
	Radius, Budget, LoadWorkers int
	Speed, Seconds              float64
	Chunks, JoinChunks          int
	FirstChunkMs                float64
	JoinMs                      *float64
	P50Ms, P95Ms, MaxMs         float64
	MovingP50Ms, MovingP95Ms    float64
	DemandChunksPerSecond       float64
	SustainedChunksPerSecond    float64
	UnservedAtEnd               int
}

// simulatePlayer walks a view disc through fresh terrain, timing each chunk until generate returns.
func simulatePlayer(opts playerOptions, generate func(world.ChunkPos)) playerResult {
	const tick = 50 * time.Millisecond
	type request struct {
		pos    world.ChunkPos
		needed time.Time
	}
	jobs := make(chan request, 4096) // Dragonfly's worker queue; a full queue defers requests

	var mu sync.Mutex
	var latencies, moving []time.Duration
	var first, joinDone time.Time
	served := map[world.ChunkPos]time.Time{}
	start := time.Now()
	end := start.Add(opts.Duration)
	var wg sync.WaitGroup
	for range opts.LoadWorkers {
		wg.Go(func() {
			for r := range jobs {
				generate(r.pos)
				now := time.Now()
				mu.Lock()
				if first.IsZero() {
					first = now
				}
				latency := now.Sub(r.needed)
				latencies = append(latencies, latency)
				served[r.pos] = now
				if r.needed.Equal(start) {
					if now.After(joinDone) {
						joinDone = now
					}
				} else {
					moving = append(moving, latency)
				}
				mu.Unlock()
			}
		})
	}

	needed := map[world.ChunkPos]time.Time{}
	issued := map[world.ChunkPos]bool{}
	var queue []world.ChunkPos
	center := world.ChunkPos{math.MaxInt32, 0}
	for now := start; now.Before(end); now = time.Now() {
		x := opts.Speed * now.Sub(start).Seconds()
		if pos := (world.ChunkPos{int32(math.Floor(x / 16)), 0}); pos != center {
			center = pos
			queue = queue[:0]
			r := int32(opts.Radius)
			for dx := -r; dx <= r; dx++ {
				for dz := -r; dz <= r; dz++ {
					p := world.ChunkPos{center[0] + dx, center[1] + dz}
					if chunkDistance(p, center) > r || issued[p] {
						continue
					}
					if _, ok := needed[p]; !ok {
						needed[p] = now
					}
					queue = append(queue, p)
				}
			}
			slices.SortStableFunc(queue, func(a, b world.ChunkPos) int {
				return int(chunkDistance(a, center) - chunkDistance(b, center))
			})
		}
		for n := 0; n < opts.Budget && len(queue) > 0; n++ {
			select {
			case jobs <- request{queue[0], needed[queue[0]]}:
				issued[queue[0]] = true
				queue = queue[1:]
			default:
				n = opts.Budget
			}
		}
		time.Sleep(time.Until(now.Add(tick)))
	}
	close(jobs)
	wg.Wait()
	joinWanted, unserved, completedInWindow := 0, 0, 0
	for p, t := range needed {
		if t.Equal(start) {
			joinWanted++
		}
		done, ok := served[p]
		if ok && done.Before(end) {
			completedInWindow++
		} else if chunkDistance(p, center) <= int32(opts.Radius) {
			unserved++
		}
	}

	result := playerResult{
		Radius: opts.Radius, Budget: opts.Budget, LoadWorkers: opts.LoadWorkers,
		Speed: opts.Speed, Seconds: opts.Duration.Seconds(),
		Chunks: len(latencies), JoinChunks: len(latencies) - len(moving),
		UnservedAtEnd:            unserved,
		DemandChunksPerSecond:    float64(len(needed)) / opts.Duration.Seconds(),
		SustainedChunksPerSecond: float64(completedInWindow) / opts.Duration.Seconds(),
	}
	if !first.IsZero() {
		result.FirstChunkMs = ms(first.Sub(start))
	}
	if result.JoinChunks == joinWanted {
		join := ms(joinDone.Sub(start))
		result.JoinMs = &join
	}
	result.P50Ms, result.P95Ms, result.MaxMs = percentile(latencies, 0.5), percentile(latencies, 0.95), percentile(latencies, 1)
	result.MovingP50Ms, result.MovingP95Ms = percentile(moving, 0.5), percentile(moving, 0.95)
	return result
}

// chunkDistance matches Dragonfly's rounded Euclidean loader radius.
func chunkDistance(a, b world.ChunkPos) int32 {
	dx, dz := float64(a[0])-float64(b[0]), float64(a[1])-float64(b[1])
	return int32(math.Round(math.Sqrt(dx*dx + dz*dz)))
}

// percentile returns the nearest-rank value in milliseconds, or zero for no samples.
func percentile(values []time.Duration, p float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return ms(sorted[max(0, int(math.Ceil(p*float64(len(sorted))))-1)])
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
