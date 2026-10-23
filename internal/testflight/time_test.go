package testflight

import (
	"testing"

	"github.com/df-mc/dragonfly/server/player"
	"github.com/df-mc/dragonfly/server/world"
)

func TestTimeCommands(t *testing.T) {
	withPlayer(t, world.GameModeCreative, func(tx *world.Tx, p *player.Player) {
		w := tx.World()
		w.StopTime()
		for _, test := range []struct {
			args string
			want int
		}{
			{"set day", 1000}, {"set noon", 6000}, {"set sunset", 12000},
			{"set night", 13000}, {"set midnight", 18000}, {"set sunrise", 23000},
			{"set 0", 0}, {"set 12345", 12345}, {"add 24000", 36345},
		} {
			runCommand(t, tx, p, "time", test.args)
			if w.Time() != test.want || w.TimeCycle() {
				t.Fatalf("/time %s: got %d, cycling %v; want %d, stopped", test.args, w.Time(), w.TimeCycle(), test.want)
			}
		}
		for _, args := range []string{"", "set", "set unknown", "set -1", "add -1", "set 2147483648", "set day extra"} {
			before := w.Time()
			runCommand(t, tx, p, "time", args)
			if w.Time() != before {
				t.Fatalf("invalid /time %q changed time to %d", args, w.Time())
			}
		}
		w.StartTime()
		runCommand(t, tx, p, "time", "set day")
		if w.Time() != 1000 || !w.TimeCycle() {
			t.Fatal("setting time changed the running daylight cycle")
		}
	})
}
