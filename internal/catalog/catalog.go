package catalog

import (
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
		"http":          overHTTP,
		"https":         overHTTP,
		base.SchemeExec: core.NewClaudeCLIExecutor(),
	}
}

func Presets() map[string]base.Backend { return base.Must(Load(config.Default)) }

func Load(document []byte) (map[string]base.Backend, error) {
	return config.Parse(document, DialectNames())
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
