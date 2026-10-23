package testflight

import (
	"math"
	"testing"

	"github.com/df-mc/dragonfly/server/cmd"
	"github.com/df-mc/dragonfly/server/entity"
	"github.com/df-mc/dragonfly/server/player"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/go-gl/mathgl/mgl64"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
)

// runCommand runs a registered chat command as p.
func runCommand(t *testing.T, tx *world.Tx, p *player.Player, name, args string) {
	t.Helper()
	c, ok := cmd.ByAlias(name)
	if !ok {
		t.Fatalf("command %q is not registered", name)
	}
	c.Execute(args, p, tx)
}

// withPlayer runs f on a player in a fresh synchronous world.
func withPlayer(t *testing.T, mode world.GameMode, f func(tx *world.Tx, p *player.Player)) {
	t.Helper()
	Register()
	w := world.Config{Synchronous: true, Entities: entity.DefaultRegistry}.New()
	t.Cleanup(func() { _ = w.Close() })
	task := w.Do(func(tx *world.Tx) {
		handle := world.EntitySpawnOpts{Position: mgl64.Vec3{0, 10, 0}}.New(player.Type, player.Config{Name: "tester", GameMode: mode})
		p := tx.AddEntity(handle).(*player.Player)
		p.Handle(&Handler{})
		f(tx, p)
	})
	if err := task.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestSpeedMultiplierAcceptsRepresentablePositiveValues(t *testing.T) {
	for _, m := range []float64{0.01, 1, 12.5, 500, 5000, 1e12} {
		if got, err := speedMultiplier(m); err != nil || got != m {
			t.Fatalf("speedMultiplier(%v) = %v, %v", m, got, err)
		}
	}
	for _, m := range []float64{0, -1, math.NaN(), math.Inf(1), math.Inf(-1), math.MaxFloat64, math.SmallestNonzeroFloat64} {
		if _, err := speedMultiplier(m); err == nil {
			t.Fatalf("accepted invalid multiplier %v", m)
		}
	}
}

func TestSpeedCommandScalesWalkingAndDirectFlight(t *testing.T) {
	withPlayer(t, world.GameModeCreative, func(tx *world.Tx, p *player.Player) {
		h := p.Handler().(*Handler)
		for _, args := range []string{"4", "500", "5000", "12.5"} {
			runCommand(t, tx, p, "speed", args)
			if h.flight.speed == 0 || p.FlightSpeed() != 0 || p.VerticalFlightSpeed() != 0 {
				t.Fatalf("/speed %s did not enable direct flight: %+v", args, h.flight)
			}
		}
		m := 12.5
		if h.flight.speed != protocol.AbilityBaseFlySpeed*flyBlocksPerSecondPerUnit*m || p.Speed() != protocol.AbilityBaseWalkSpeed*m {
			t.Fatalf("walking and flight did not use the same multiplier: walk %v, flight %v", p.Speed(), h.flight.speed)
		}
		for _, args := range []string{"abc", "0", "-1", "NaN", "+Inf", "1e100", "500 extra", "reset extra"} {
			runCommand(t, tx, p, "speed", args)
			if h.flight.speed != protocol.AbilityBaseFlySpeed*flyBlocksPerSecondPerUnit*m || p.Speed() != protocol.AbilityBaseWalkSpeed*m {
				t.Fatalf("invalid /speed %q changed movement", args)
			}
		}
		p.StartFlying()
		for _, args := range []string{"reset", "", "1"} {
			runCommand(t, tx, p, "speed", "4")
			p.SetVelocity(mgl64.Vec3{10, 1, 0})
			runCommand(t, tx, p, "speed", args)
			if h.flight.speed != 0 || p.FlightSpeed() != protocol.AbilityBaseFlySpeed ||
				p.VerticalFlightSpeed() != protocol.AbilityBaseVerticalFlySpeed || p.Speed() != protocol.AbilityBaseWalkSpeed || p.Velocity() != (mgl64.Vec3{}) {
				t.Fatalf("/speed %s did not restore normal movement", args)
			}
		}
		if _, ok := cmd.ByAlias("flyspeed"); ok {
			t.Fatal("the removed /flyspeed command is still available")
		}
	})
}

// A speed set mid-sprint must return to the scaled walk speed once Dragonfly undoes the sprint.
func TestSpeedCommandKeepsSprintScaling(t *testing.T) {
	withPlayer(t, world.GameModeCreative, func(tx *world.Tx, p *player.Player) {
		p.StartSprinting()
		runCommand(t, tx, p, "speed", "4")
		p.StopSprinting()
		if want := protocol.AbilityBaseWalkSpeed * 4; math.Abs(p.Speed()-want) > 1e-9 {
			t.Fatalf("walk speed after sprint = %v, want %v", p.Speed(), want)
		}
	})
}

func TestFlyCommandTogglesFlight(t *testing.T) {
	withPlayer(t, world.GameModeAdventure, func(tx *world.Tx, p *player.Player) {
		runCommand(t, tx, p, "fly", "")
		if !p.GameMode().AllowsFlying() || p.GameMode().AllowsEditing() {
			t.Fatal("/fly must allow flight and keep the rest of adventure mode")
		}
		p.StartFlying()
		runCommand(t, tx, p, "fly", "")
		if p.GameMode() != world.GameModeAdventure || p.Flying() {
			t.Fatalf("second /fly left mode %T, flying %v", p.GameMode(), p.Flying())
		}
	})
	withPlayer(t, world.GameModeCreative, func(tx *world.Tx, p *player.Player) {
		runCommand(t, tx, p, "fly", "")
		if p.GameMode() != world.GameModeCreative {
			t.Fatalf("/fly changed creative to %T", p.GameMode())
		}
	})
}
