// Package testflight provides movement and time controls for terrain testing.
package testflight

import "github.com/df-mc/dragonfly/server/player"

// Handler owns flight settings on the player's world transaction and discards them on quit.
type Handler struct {
	player.NopHandler
	flight flySpeedState
}
