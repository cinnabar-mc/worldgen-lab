// steel-server runs a standalone Bedrock server with SteelMC overworld generation.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"

	"github.com/cinnabar-mc/worldgen-lab/generator"
	"github.com/cinnabar-mc/worldgen-lab/internal/testflight"
	"github.com/df-mc/dragonfly/server"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/google/uuid"
)

// main reports startup failures with a nonzero exit status.
func main() {
	if err := run(); err != nil {
		slog.Error("Steel server stopped", "error", err)
		os.Exit(1)
	}
}

// run configures persistence, generation, networking and graceful Dragonfly shutdown.
func run() error {
	addr := flag.String("addr", "127.0.0.1:19132", "UDP listen address")
	dir := flag.String("dir", "worlds/steel", "world and player data directory")
	seed := flag.Int64("seed", 1, "signed 64-bit world seed")
	offline := flag.Bool("offline", false, "allow unauthenticated local test clients (loopback only)")
	workers := flag.Int("workers", generator.DefaultWorkers(), "Dragonfly and native generation workers")
	radius := flag.Int("radius", 12, "maximum player chunk radius")
	flag.Parse()
	if *workers < 1 || *radius < 1 {
		return errors.New("workers and radius must be positive")
	}
	if *offline {
		host, _, err := net.SplitHostPort(*addr)
		if err != nil || !net.ParseIP(host).IsLoopback() {
			return errors.New("-offline requires a loopback IP in -addr")
		}
	}
	if err := checkSeed(*dir, *seed); err != nil {
		return err
	}
	log := slog.Default()
	gen, err := (generator.Config{Workers: *workers}).New(context.Background(), *seed, log)
	if err != nil {
		return err
	}
	defer gen.Close()
	config := server.DefaultConfig()
	config.Network.Address = *addr
	config.Server.AuthEnabled = !*offline
	config.Server.Name = "SteelMC terrain"
	config.World.Folder = filepath.Join(*dir, "world")
	config.Players.Folder = filepath.Join(*dir, "players")
	config.Resources.Folder = filepath.Join(*dir, "resources")
	config.Players.MaximumChunkRadius = *radius
	conf, err := config.Config(log)
	if err != nil {
		return err
	}
	conf.ChunkLoadWorkers = *workers
	// Dragonfly's defaults of 4 per tick and 8 open transactions cap a player at 80 chunks/s.
	conf.ChunkLoadsPerTick, conf.MaxChunkTransactions = 32, 64
	conf.Generator = func(dim world.Dimension) world.Generator {
		if dim == world.Overworld {
			return gen
		}
		return world.NopGenerator{}
	}
	settings := conf.WorldProvider.Settings()
	settings.Spawn = gen.DefaultSpawn(world.Overworld)
	settings.DefaultGameMode = world.GameModeCreative
	conf.WorldProvider.SaveSettings(settings)
	testflight.Register()
	var srv *server.Server
	for i, listen := range conf.Listeners {
		conf.Listeners[i] = testflight.WrapListener(listen, func(id uuid.UUID) (*world.EntityHandle, bool) {
			return srv.Player(id)
		})
	}
	srv = conf.New()
	srv.CloseOnProgramEnd()
	srv.Listen()
	for p := range srv.Accept() {
		p.Handle(&testflight.Handler{})
	}
	log.Info("generation totals", "stats", gen.Stats())
	return nil
}

// worldIdentity records the inputs used to create a saved terrain directory.
type worldIdentity struct {
	Seed      *int64 `json:"seed"`
	Generator string `json:"generator"`
}

// checkSeed prevents reopening generated chunks with a different seed or generator revision.
func checkSeed(dir string, seed int64) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	path := filepath.Join(dir, "generator.json")
	want := worldIdentity{&seed, "steel-da3fb73f-geyser-81700ab5-stream-v1"}
	data, err := os.ReadFile(path)
	if err == nil {
		var got worldIdentity
		if err = json.Unmarshal(data, &got); err != nil {
			return err
		}
		if got.Seed == nil || *got.Seed != seed || got.Generator != want.Generator {
			return fmt.Errorf("world seed or generator differs from %s; choose a new -dir", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err = os.Stat(filepath.Join(dir, "world", "db")); err == nil {
		return errors.New("world has no generator metadata; choose a new -dir")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	data, err = json.MarshalIndent(want, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append(data, '\n'))
	return errors.Join(writeErr, file.Close())
}
