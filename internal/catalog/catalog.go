// Package catalog wires the protocols dragoman speaks to the service table
// built out of them. The protocol set is code, because dragoman owns it; the
// service table is data, because nobody can enumerate in advance the endpoints
// people want to reach.
package catalog

import (
	"sort"

	"github.com/matthijn/dragoman/internal/base"
	"github.com/matthijn/dragoman/internal/config"
	"github.com/matthijn/dragoman/internal/core"
	"github.com/matthijn/dragoman/internal/dialect"
	"github.com/matthijn/dragoman/internal/dialect/anthropicmessages"
	"github.com/matthijn/dragoman/internal/dialect/geminiinteractions"
	"github.com/matthijn/dragoman/internal/dialect/openaicompletions"
	"github.com/matthijn/dragoman/internal/dialect/openairesponses"
)

// New builds the catalog from the embedded service table.
func New() core.Catalog {
	return core.Catalog{Dialects: Dialects(), Presets: Presets()}
}

// Presets is the shipped service table. A table dragoman ships with cannot be
// wrong at runtime, so a failure to parse it is a broken build rather than an
// error a caller could handle.
func Presets() map[string]base.Backend { return base.Must(Load(config.Default)) }

// Load parses a service table against the protocols dragoman speaks.
func Load(document []byte) (map[string]base.Backend, error) {
	return config.Parse(document, DialectNames())
}

func Dialects() map[string]dialect.Dialect {
	table := map[string]dialect.Dialect{}
	for _, spoken := range []dialect.Dialect{
		openairesponses.Dialect(),
		openaicompletions.Dialect(),
		openaicompletions.ChatDialect(),
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
