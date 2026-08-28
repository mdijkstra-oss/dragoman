package config

import (
	"bytes"
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

type Mode string

const (
	ModeAppend   Mode = "append"
	ModeOverride Mode = "override"
)

// The one key at the top of a document that does not name a service, which is
// why no service may be called this.
const ModeKey = "mode"

type Table struct {
	// Whether the services join the built-in ones or stand for all of them.
	Mode Mode

	Services map[string]base.Backend
}

func Parse(document []byte, protocols []string) (Table, error) {
	var entries map[string]yaml.Node
	if err := yaml.Unmarshal(document, &entries); err != nil {
		return Table{}, fmt.Errorf("config: %w", err)
	}
	mode, err := modeOf(entries)
	if err != nil {
		return Table{}, err
	}
	delete(entries, ModeKey)
	if len(entries) == 0 {
		return Table{}, fmt.Errorf("config: no services defined")
	}

	known := map[string]bool{}
	for _, protocol := range protocols {
		known[protocol] = true
	}

	services := make(map[string]base.Backend, len(entries))
	for _, name := range sorted(entries) {
		var entry service
		node := entries[name]
		if err := node.Decode(&entry); err != nil {
			return Table{}, fmt.Errorf("config: service %q: %w", name, err)
		}
		backend, err := resolve(name, entry, known, protocols)
		if err != nil {
			return Table{}, err
		}
		services[name] = backend
	}
	return Table{Mode: mode, Services: services}, nil
}

// Absent, a document adds to what is already served: the reading under which a
// file naming one service leaves the others alone.
func modeOf(entries map[string]yaml.Node) (Mode, error) {
	node, set := entries[ModeKey]
	if !set {
		return ModeAppend, nil
	}
	var value string
	if err := node.Decode(&value); err != nil {
		return "", fmt.Errorf("config: %s is not a name (one of: %s, %s)", ModeKey, ModeAppend, ModeOverride)
	}
	if Mode(value) != ModeAppend && Mode(value) != ModeOverride {
		return "", fmt.Errorf("config: %s is %q (one of: %s, %s)", ModeKey, value, ModeAppend, ModeOverride)
	}
	return Mode(value), nil
}

type service struct {
	Endpoint                   string `yaml:"endpoint"`
	Protocol                   string `yaml:"protocol"`
	Auth                       string `yaml:"auth"`
	SchemaAsPrompt             bool   `yaml:"schema_as_prompt,omitempty"`
	MaxOutputTokensAs          string `yaml:"max_output_tokens_as,omitempty"`
	OmitDefaultMaxOutputTokens bool   `yaml:"omit_default_max_output_tokens,omitempty"`
}

// Format writes a table back as the document that would produce it, so what an
// instance serves can be read in the same shape it is configured in. What comes
// out is the whole of what was served, and says so, so deleting a service from
// it and passing it back leaves that service unserved.
func Format(table map[string]base.Backend) ([]byte, error) {
	document := &yaml.Node{Kind: yaml.MappingNode, Content: []*yaml.Node{
		{Kind: yaml.ScalarNode, Value: ModeKey},
		{Kind: yaml.ScalarNode, Value: string(ModeOverride)},
	}}
	for _, name := range sorted(table) {
		entry := &yaml.Node{}
		if err := entry.Encode(written(table[name])); err != nil {
			return nil, fmt.Errorf("config: %s: %w", name, err)
		}
		document.Content = append(document.Content,
			&yaml.Node{Kind: yaml.ScalarNode, Value: name}, entry)
	}

	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return out.Bytes(), nil
}

func written(backend base.Backend) service {
	return service{
		Endpoint:                   backend.Endpoint,
		Protocol:                   backend.Dialect,
		Auth:                       backend.KeyEnv,
		SchemaAsPrompt:             backend.SchemaAsPrompt,
		MaxOutputTokensAs:          backend.MaxOutputTokensAs,
		OmitDefaultMaxOutputTokens: backend.OmitDefaultMaxOutputTokens,
	}
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
	// The variable is required and its value is not: an unset key fails when
	// that service is called.
	if entry.Auth == "" {
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
	if entry.OmitDefaultMaxOutputTokens && entry.Protocol != "openai-responses" {
		return base.Backend{}, fmt.Errorf("config: service %q sets omit_default_max_output_tokens on protocol %q (only openai-responses can omit Dragoman's synthesized default)",
			name, entry.Protocol)
	}
	return base.Backend{
		Name:                       name,
		Dialect:                    entry.Protocol,
		Endpoint:                   entry.Endpoint,
		KeyEnv:                     entry.Auth,
		SchemaAsPrompt:             entry.SchemaAsPrompt,
		MaxOutputTokensAs:          entry.MaxOutputTokensAs,
		OmitDefaultMaxOutputTokens: entry.OmitDefaultMaxOutputTokens,
	}, nil
}

func sorted[V any](table map[string]V) []string {
	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
