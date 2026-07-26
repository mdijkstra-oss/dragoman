// Package catalog wires the dialects dragoman knows to the presets that name
// backends built out of them. The presets are a convenience and must stay one:
// everything here is expressible in flags, so an endpoint nobody anticipated is
// a command line rather than a patch.
package catalog

import (
	"sort"

	"github.com/matthijn/dragoman/internal/base"
	"github.com/matthijn/dragoman/internal/core"
	"github.com/matthijn/dragoman/internal/dialect"
	"github.com/matthijn/dragoman/internal/dialect/anthropicmessages"
	"github.com/matthijn/dragoman/internal/dialect/geminiinteractions"
	"github.com/matthijn/dragoman/internal/dialect/openaicompletions"
	"github.com/matthijn/dragoman/internal/dialect/openairesponses"
)

// AnthropicMaxTokensFallback is the cap sent when a backend requires one and
// neither flag nor body supplied it. A cap that has to be guessed should err
// toward not being the thing that ruins the answer, and on a reasoning model
// the cap covers thinking and output together, so a small one truncates
// mid-thought.
const AnthropicMaxTokensFallback = 20000

func New() core.Catalog {
	return core.Catalog{Dialects: Dialects(), Presets: Presets()}
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

// Presets names backends. A preset carries the backend's quirks record beside
// its endpoint and dialect.
func Presets() map[string]base.Backend {
	fallback := AnthropicMaxTokensFallback
	return map[string]base.Backend{
		"openai": {
			Name:     "openai",
			Dialect:  openairesponses.Name,
			Endpoint: "https://api.openai.com/v1",
			KeyEnv:   "OPENAI_API_KEY",
		},
		"anthropic": {
			Name:     "anthropic",
			Dialect:  anthropicmessages.Name,
			Endpoint: "https://api.anthropic.com/v1",
			KeyEnv:   "ANTHROPIC_API_KEY",
			Quirks: base.Quirks{
				RequiresMaxTokens: true,
				MaxTokensFallback: &fallback,
				// A non-streaming request under the guessed cap may
				// legitimately run long, so this backend is always streamed
				// upstream. Folding that stream is defined to produce exactly
				// the buffered body, so the caller cannot tell.
				StreamUpstream: true,
			},
		},
		"gemini": {
			Name:     "gemini",
			Dialect:  geminiinteractions.Name,
			Endpoint: "https://generativelanguage.googleapis.com/v1beta",
			KeyEnv:   "GEMINI_API_KEY",
		},
		"deepseek": {
			Name:     "deepseek",
			Dialect:  openaicompletions.Name,
			Endpoint: "https://api.deepseek.com",
			KeyEnv:   "DEEPSEEK_API_KEY",
			Quirks: base.Quirks{
				// No schema mode: a schema here is dropped and reported, and
				// the caller gets prose.
				NoStructuredSchema: true,
				MaxTokensField:     "max_tokens",
			},
		},
		"openai-chat": {
			Name:     "openai-chat",
			Dialect:  openaicompletions.Name,
			Endpoint: "https://api.openai.com/v1",
			KeyEnv:   "OPENAI_API_KEY",
			Quirks: base.Quirks{
				// Same wire shape as DeepSeek, different spelling of the cap,
				// and usage while streaming has to be asked for.
				MaxTokensField:    "max_completion_tokens",
				StreamUsageOption: true,
			},
		},
		"openrouter": {
			Name:     "openrouter",
			Dialect:  openaicompletions.Name,
			Endpoint: "https://openrouter.ai/api/v1",
			KeyEnv:   "OPENROUTER_API_KEY",
			// The empty record is the property the abstraction protects.
		},
	}
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
