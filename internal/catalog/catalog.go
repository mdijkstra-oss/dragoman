package catalog

import (
	"maps"
	"sort"

	"github.com/mdijkstra-oss/dragoman/internal/base"
	"github.com/mdijkstra-oss/dragoman/internal/config"
	"github.com/mdijkstra-oss/dragoman/internal/core"
	"github.com/mdijkstra-oss/dragoman/internal/dialect"
	"github.com/mdijkstra-oss/dragoman/internal/dialect/anthropicmessages"
	"github.com/mdijkstra-oss/dragoman/internal/dialect/geminiinteractions"
	"github.com/mdijkstra-oss/dragoman/internal/dialect/openaicompletions"
	"github.com/mdijkstra-oss/dragoman/internal/dialect/openairesponses"
)

func New() core.Catalog {
	return core.Catalog{Dialects: Dialects(), Presets: Presets(), Executors: Executors()}
}

func Executors() core.Executors {
	overHTTP := core.NewHTTPExecutor()
	return core.Executors{
		"http":  overHTTP,
		"https": overHTTP,
	}
}

func Presets() map[string]base.Backend {
	return base.Must(config.Parse(config.Default, DialectNames())).Services
}

// A document adds to the built-in services unless it sets mode: override, and a
// name defined in both is the document's — so an entry can point a shipped name
// at another endpoint.
func Load(document []byte) (map[string]base.Backend, error) {
	parsed, err := config.Parse(document, DialectNames())
	if err != nil {
		return nil, err
	}
	if parsed.Mode == config.ModeOverride {
		return parsed.Services, nil
	}
	table := Presets()
	maps.Copy(table, parsed.Services)
	return table, nil
}

func Dialects() map[string]dialect.Dialect {
	table := map[string]dialect.Dialect{}
	for _, spoken := range []dialect.Dialect{
		openairesponses.Dialect(),
		openaicompletions.Dialect(),
		anthropicmessages.Dialect(),
		geminiinteractions.Dialect(),
	} {
		table[spoken.Name] = spoken
	}
	return table
}

func DialectNames() []string { return sorted(Dialects()) }

func PresetNames() []string { return sorted(Presets()) }

func sorted[V any](table map[string]V) []string {
	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
