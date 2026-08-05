package config

import (
	_ "embed"
	"fmt"
	"slices"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/mdijkstra-oss/dragoman/internal/base"
)

//go:embed dragoman.yaml
var Default []byte

func Parse(document []byte, protocols []string) (map[string]base.Backend, error) {
	var services map[string]service
	if err := yaml.Unmarshal(document, &services); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if len(services) == 0 {
		return nil, fmt.Errorf("config: no services defined")
	}

	known := map[string]bool{}
	for _, protocol := range protocols {
		known[protocol] = true
	}

	table := make(map[string]base.Backend, len(services))
	for _, name := range sorted(services) {
		backend, err := resolve(name, services[name], known, protocols)
		if err != nil {
			return nil, err
		}
		table[name] = backend
	}
	return table, nil
}

type service struct {
	Endpoint          string `yaml:"endpoint"`
	Protocol          string `yaml:"protocol"`
	Auth              string `yaml:"auth"`
	SchemaAsPrompt    bool   `yaml:"schema_as_prompt"`
	MaxOutputTokensAs string `yaml:"max_output_tokens_as"`
}

// Both quirks belong to the one wire format several companies serve, and
// disagree about.
const quirkProtocol = "openai-completions"

var capFields = []string{"max_completion_tokens", "max_tokens"}

func resolve(name string, entry service, known map[string]bool, protocols []string) (base.Backend, error) {
	if entry.Protocol == "" {
		return base.Backend{}, fmt.Errorf("config: service %q has no protocol (one of: %s)", name, strings.Join(protocols, ", "))
	}
	if !known[entry.Protocol] {
		return base.Backend{}, fmt.Errorf("config: service %q names unknown protocol %q (one of: %s)",
			name, entry.Protocol, strings.Join(protocols, ", "))
	}
	if entry.Endpoint == "" {
		return base.Backend{}, fmt.Errorf("config: service %q has no endpoint", name)
	}
	// A CLI signed in interactively holds its own credential and names no
	// variable. Elsewhere the variable is required and its value is not: an
	// unset key fails when that service is called.
	if entry.Auth == "" && !base.IsSpawned(entry.Endpoint) {
		return base.Backend{}, fmt.Errorf("config: service %q has no auth (name the environment variable holding the key)", name)
	}
	if entry.SchemaAsPrompt && entry.Protocol != quirkProtocol {
		return base.Backend{}, fmt.Errorf("config: service %q sets schema_as_prompt on protocol %q (only %s has a JSON mode to fall back to)",
			name, entry.Protocol, quirkProtocol)
	}
	if entry.MaxOutputTokensAs != "" {
		if entry.Protocol != quirkProtocol {
			return base.Backend{}, fmt.Errorf("config: service %q sets max_output_tokens_as on protocol %q (only %s spells the cap two ways)",
				name, entry.Protocol, quirkProtocol)
		}
		if !slices.Contains(capFields, entry.MaxOutputTokensAs) {
			return base.Backend{}, fmt.Errorf("config: service %q sets max_output_tokens_as to %q (one of: %s)",
				name, entry.MaxOutputTokensAs, strings.Join(capFields, ", "))
		}
	}
	return base.Backend{
		Name:              name,
		Dialect:           entry.Protocol,
		Endpoint:          entry.Endpoint,
		KeyEnv:            entry.Auth,
		SchemaAsPrompt:    entry.SchemaAsPrompt,
		MaxOutputTokensAs: entry.MaxOutputTokensAs,
	}, nil
}

func sorted(services map[string]service) []string {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
