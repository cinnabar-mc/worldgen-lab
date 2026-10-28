package generator

import (
	"bytes"
	"compress/gzip"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/df-mc/dragonfly/server/world/chunk"
)

//go:embed mappings/table.json.gz
var mappingData []byte

// bedrockState is Geyser's name and property representation of a Bedrock state.
type bedrockState struct {
	Name   string         `json:"name"`
	States map[string]any `json:"states"`
}

// mappingTable contains Java state keys and Java biome names, never Java numeric IDs.
type mappingTable struct {
	Blocks map[string]bedrockState `json:"blocks"`
	Biomes map[string]uint32       `json:"biomes"`
}

var loadTable = sync.OnceValues(func() (*mappingTable, error) {
	reader, err := gzip.NewReader(bytes.NewReader(mappingData))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	var table mappingTable
	if err = json.NewDecoder(reader).Decode(&table); err != nil {
		return nil, err
	}
	return &table, nil
})

// translatedState carries the runtime ID and whether this state used a fallback.
type translatedState struct {
	id          uint32
	waterlogged bool
	fallback    bool
}

// translator resolves mappings for the particular registry belonging to a target chunk.
type translator struct {
	registry                 chunk.BlockRegistry
	table                    *mappingTable
	log                      *slog.Logger
	states                   sync.Map
	missingBiomes            sync.Map
	prototypes               map[string]map[string]any
	air, stone, water        uint32
	unmapped, unmappedBiomes *atomic.Uint64
}

// newTranslator learns the NBT property types from Dragonfly's finalized block registry.
func newTranslator(registry chunk.BlockRegistry, table *mappingTable, log *slog.Logger, states, biomes *atomic.Uint64) (*translator, error) {
	t := &translator{registry: registry, table: table, log: log, prototypes: make(map[string]map[string]any), unmapped: states, unmappedBiomes: biomes}
	for i := 0; i < registry.BlockCount(); i++ {
		name, props, ok := registry.RuntimeIDToState(uint32(i))
		if ok {
			t.prototypes[name] = props
		}
	}
	t.air = registry.AirRuntimeID()
	var ok bool
	if t.stone, ok = t.lookup("minecraft:stone"); !ok {
		return nil, fmt.Errorf("registry has no mapped stone")
	}
	if t.water, ok = t.lookup("minecraft:water[level=0]"); !ok {
		return nil, fmt.Errorf("registry has no mapped water")
	}
	return t, nil
}

// lookup matches a Java key to a Bedrock state using the registry's exact property types.
func (t *translator) lookup(java string) (uint32, bool) {
	state, ok := t.table.Blocks[java]
	if !ok {
		return 0, false
	}
	prototype, exists := t.prototypes[state.Name]
	if !exists {
		return 0, false
	}
	props := make(map[string]any, len(state.States))
	for key, value := range state.States {
		if number, numeric := value.(float64); numeric {
			switch prototype[key].(type) {
			case bool:
				if number != 0 && number != 1 {
					return 0, false
				}
				value = number == 1
			case uint8:
				value = uint8(number)
			case int32:
				value = int32(number)
			default:
				return 0, false
			}
		}
		props[key] = value
	}
	// Mappings older than a block's newest properties omit them; use the defaults as the game does.
	if defaults, ok := t.registry.DefaultProperties(state.Name); ok {
		for key, value := range defaults {
			if _, set := props[key]; !set {
				props[key] = value
			}
		}
	}
	return t.registry.StateToRuntimeID(state.Name, props)
}

// translate caches one lookup per Java palette state and logs each fallback once.
func (t *translator) translate(java, defaultState string, solid bool) translatedState {
	if cached, ok := t.states.Load(java); ok {
		return cached.(translatedState)
	}
	id, exact := t.lookup(java)
	if !exact {
		var ok bool
		if id, ok = t.lookup(defaultState); !ok {
			id = t.air
			if solid {
				id = t.stone
			}
		}
	}
	state := translatedState{id: id, waterlogged: strings.Contains(java, "waterlogged=true"), fallback: !exact}
	actual, loaded := t.states.LoadOrStore(java, state)
	if !loaded && !exact {
		t.unmapped.Add(1)
		t.log.Warn("Java state uses fallback", "state", java, "default", defaultState, "solid", solid)
	}
	return actual.(translatedState)
}

// biome resolves a Java biome name and reports each missing name once.
func (t *translator) biome(name string) uint32 {
	if id, ok := t.table.Biomes[name]; ok {
		return id
	}
	if _, loaded := t.missingBiomes.LoadOrStore(name, true); !loaded {
		t.unmappedBiomes.Add(1)
		t.log.Warn("Java biome uses plains fallback", "biome", name)
	}
	return t.table.Biomes["minecraft:plains"]
}
