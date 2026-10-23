package testflight

import (
	"errors"
	"math"

	"github.com/df-mc/dragonfly/server/player"
	"github.com/go-gl/mathgl/mgl64"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
)

const flightTicksPerSecond = 20

// flySpeedValue accepts positive speeds that fit the motion packet without clamping.
func flySpeedValue(speed float64) (float64, error) {
	motion := float32(speed / flightTicksPerSecond)
	if math.IsNaN(speed) || math.IsInf(speed, 0) || speed <= 0 ||
		math.IsInf(float64(motion), 0) || motion == 0 {
		return 0, errors.New("flight speed must be positive, finite, and fit a motion packet")
	}
	return speed, nil
}

// flySpeedState lives on the player's world owner and is discarded on disconnect.
type flySpeedState struct {
	speed    float64
	previous [2]float64
}

// set saves and suppresses ability acceleration while motion flight owns flight.
// Turning it off restores the original abilities and stops any active flight motion.
func (s *flySpeedState) set(p *player.Player, speed float64) {
	if speed == 0 {
		if s.speed == 0 {
			return
		}
		p.SetFlightSpeed(s.previous[0])
		p.SetVerticalFlightSpeed(s.previous[1])
		if p.Flying() {
			p.SetVelocity(mgl64.Vec3{})
		}
	} else if s.speed == 0 {
		s.previous = [2]float64{p.FlightSpeed(), p.VerticalFlightSpeed()}
		p.SetFlightSpeed(0)
		p.SetVerticalFlightSpeed(0)
	}
	s.speed = speed
}

// tick sends one target velocity after Dragonfly has handled this tick's input.
func (s *flySpeedState) tick(p *player.Player, input *packet.PlayerAuthInput) {
	if s.speed == 0 || !p.Flying() || p.Dead() {
		return
	}
	if velocity, ok := flySpeedVelocity(input, s.speed); ok {
		p.SetVelocity(velocity)
	}
}

// flySpeedVelocity rotates raw strafe/forward input into blocks per tick.
// Partial input stays proportional; diagonals are limited to the selected horizontal
// speed. Jump and sneak steer vertically at that same speed and cancel when both are held.
func flySpeedVelocity(input *packet.PlayerAuthInput, speed float64) (mgl64.Vec3, bool) {
	strafe, forward, yaw := float64(input.RawMoveVector[0]), float64(input.RawMoveVector[1]), float64(input.Yaw)
	for _, value := range [...]float64{strafe, forward, yaw} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return mgl64.Vec3{}, false
		}
	}
	if length := math.Hypot(strafe, forward); length > 1 {
		strafe, forward = strafe/length, forward/length
	}
	jump := input.InputData.Load(packet.InputFlagJumping) || input.InputData.Load(packet.InputFlagJumpDown) ||
		input.InputData.Load(packet.InputFlagJumpCurrentRaw) || input.InputData.Load(packet.InputFlagAscend) ||
		input.InputData.Load(packet.InputFlagWantUp)
	sneak := input.InputData.Load(packet.InputFlagSneaking) || input.InputData.Load(packet.InputFlagSneakDown) ||
		input.InputData.Load(packet.InputFlagSneakCurrentRaw) || input.InputData.Load(packet.InputFlagDescend) ||
		input.InputData.Load(packet.InputFlagWantDown)
	vertical := 0.0
	if jump != sneak {
		if jump {
			vertical = 1
		} else {
			vertical = -1
		}
	}
	sin, cos := math.Sincos(yaw * math.Pi / 180)
	motion := speed / flightTicksPerSecond
	return mgl64.Vec3{(cos*strafe - sin*forward) * motion, vertical * motion, (sin*strafe + cos*forward) * motion}, true
}
