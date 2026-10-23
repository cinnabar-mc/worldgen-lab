package testflight

import (
	"errors"

	"github.com/df-mc/dragonfly/server/cmd"
	"github.com/df-mc/dragonfly/server/player"
	"github.com/df-mc/dragonfly/server/world"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
)

const (
	// Dragonfly's sprint scales the movement attribute by this on start and divides it back on stop.
	sprintSpeedFactor = 1.3
	// Steady vanilla speeds per unit of FlySpeed and movement attribute, from the client's flight
	// and walking physics: 0.05 flies 10.9 blocks/s, 0.1 walks 4.32 blocks/s.
	flyBlocksPerSecondPerUnit  = 217.8
	walkBlocksPerSecondPerUnit = 43.17
)

// Register makes the testing commands available to every player.
func Register() {
	cmd.Register(cmd.New("speed", "Scales walking and direct flight speed; reset restores normal movement", nil, speedReset{}, speedSet{}))
	cmd.Register(cmd.New("fly", "Toggles being allowed to fly", nil, flyToggle{}))
	cmd.Register(cmd.New("time", "Sets or adds to the world's time", nil, timeSetPreset{}, timeSetTicks{}, timeAdd{}))
}

// speedMultiplier accepts positive multipliers whose flight velocity fits a motion packet.
func speedMultiplier(m float64) (float64, error) {
	if _, err := flySpeedValue(protocol.AbilityBaseFlySpeed * flyBlocksPerSecondPerUnit * m); err != nil {
		return 0, errors.New("speed multiplier must be positive, finite, and fit a motion packet")
	}
	return m, nil
}

// applySpeed scales walking and direct flight, or restores normal abilities at x1.
func applySpeed(p *player.Player, m float64) error {
	h, ok := p.Handler().(*Handler)
	if !ok {
		return errors.New("speed controls are unavailable for this player")
	}
	if m == 1 {
		h.flight.set(p, 0)
		p.SetFlightSpeed(protocol.AbilityBaseFlySpeed)
		p.SetVerticalFlightSpeed(protocol.AbilityBaseVerticalFlySpeed)
	} else {
		h.flight.set(p, protocol.AbilityBaseFlySpeed*flyBlocksPerSecondPerUnit*m)
	}
	walk := protocol.AbilityBaseWalkSpeed * m
	if p.Sprinting() {
		walk *= sprintSpeedFactor
	}
	p.SetSpeed(walk)
	return nil
}

// reportSpeed prints the walking and direct flight rates, or confirms a reset.
func reportSpeed(o *cmd.Output, m float64) {
	if m == 1 {
		o.Print("Speed reset to normal walking and flight")
		return
	}
	fly := protocol.AbilityBaseFlySpeed * m * flyBlocksPerSecondPerUnit
	o.Printf("Speed x%g: direct flight %g blocks/s, walking ~%.1f blocks/s", m, fly, protocol.AbilityBaseWalkSpeed*m*walkBlocksPerSecondPerUnit)
}

type speedSet struct {
	Multiplier cmd.Optional[float64] `cmd:"multiplier"`
}

// Run applies a speed multiplier, defaulting to normal movement.
func (s speedSet) Run(src cmd.Source, o *cmd.Output, _ *world.Tx) {
	p, ok := src.(*player.Player)
	if !ok {
		o.Error("only players can change their speed")
		return
	}
	m, err := speedMultiplier(s.Multiplier.LoadOr(1))
	if err != nil {
		o.Error(err)
		return
	}
	if err := applySpeed(p, m); err != nil {
		o.Error(err)
		return
	}
	reportSpeed(o, m)
}

type speedReset struct {
	Reset cmd.SubCommand `cmd:"reset"`
}

// Run restores the default walking and flight abilities.
func (speedReset) Run(src cmd.Source, o *cmd.Output, tx *world.Tx) {
	speedSet{}.Run(src, o, tx)
}

// flightMode lets a game mode without flight fly. Player data cannot store it and saves survival.
type flightMode struct{ world.GameMode }

// AllowsFlying grants flight while retaining the original game mode rules.
func (flightMode) AllowsFlying() bool { return true }

type flyToggle struct{}

// Run toggles flight permission without changing the other game mode rules.
func (flyToggle) Run(src cmd.Source, o *cmd.Output, _ *world.Tx) {
	p, ok := src.(*player.Player)
	if !ok {
		o.Error("only players can fly")
		return
	}
	switch mode := p.GameMode().(type) {
	case flightMode:
		p.SetGameMode(mode.GameMode)
		o.Print("Flight disabled")
	default:
		if mode.AllowsFlying() {
			o.Print("This game mode already allows flight")
			return
		}
		p.SetGameMode(flightMode{mode})
		o.Print("Flight enabled: double-tap jump to fly")
	}
}
