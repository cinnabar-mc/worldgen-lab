package testflight

import (
	"slices"

	"github.com/df-mc/dragonfly/server/cmd"
	"github.com/df-mc/dragonfly/server/world"
)

var timePresets = map[timePreset]int{
	"day": 1000, "noon": 6000, "sunset": 12000,
	"night": 13000, "midnight": 18000, "sunrise": 23000,
}

type timePreset string

// Type identifies the named time choices in command suggestions.
func (timePreset) Type() string { return "TimeSpec" }

// Options derives the available names from the preset definitions.
func (timePreset) Options(cmd.Source) []string {
	names := make([]string, 0, len(timePresets))
	for name := range timePresets {
		names = append(names, string(name))
	}
	slices.Sort(names)
	return names
}

type timeSetPreset struct {
	Set  cmd.SubCommand `cmd:"set"`
	Time timePreset     `cmd:"time"`
}

// Run sets a named time without changing whether the daylight cycle runs.
func (s timeSetPreset) Run(_ cmd.Source, o *cmd.Output, tx *world.Tx) {
	setWorldTime(o, tx, timePresets[s.Time])
}

type timeSetTicks struct {
	Set   cmd.SubCommand `cmd:"set"`
	Ticks int32          `cmd:"ticks"`
}

// Run sets a nonnegative tick count in the command's world.
func (s timeSetTicks) Run(_ cmd.Source, o *cmd.Output, tx *world.Tx) {
	setWorldTime(o, tx, int(s.Ticks))
}

type timeAdd struct {
	Add   cmd.SubCommand `cmd:"add"`
	Ticks int32          `cmd:"ticks"`
}

// Run advances the world's time by a nonnegative tick count.
func (a timeAdd) Run(_ cmd.Source, o *cmd.Output, tx *world.Tx) {
	if a.Ticks < 0 {
		o.Error("time must be nonnegative")
		return
	}
	setWorldTime(o, tx, tx.World().Time()+int(a.Ticks))
}

// setWorldTime validates a tick count and sends the new time to the world's viewers.
func setWorldTime(o *cmd.Output, tx *world.Tx, ticks int) {
	if ticks < 0 {
		o.Error("time must be nonnegative")
		return
	}
	tx.World().SetTime(ticks)
	o.Printf("Time set to %d", ticks)
}
