// Package config parses the service table. A service is a protocol at a URL
// with a key in the environment, which is the whole of what an instance gets to
// decide: where a field lands on the wire belongs to the protocol, and a table
// that could relocate one would be a config file making a structural choice.
package config

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/matthijn/dragoman/internal/base"
)

// Default is the shipped table, embedded so that the services dragoman knows
// out of the box are parsed by the same code that parses an operator's file.
// A Go literal beside it could disagree with it; this cannot.
//
//go:embed dragoman.yaml
var Default []byte

// Parse reads a service table. protocols is the set of names dragoman speaks —
// passed in rather than imported, because the table is data and the protocol
// list is code.
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

// service is the wire shape of one entry. The keys are the three a backend
// always had, plus the one thing an endpoint knows that no protocol can.
type service struct {
	Endpoint string `yaml:"endpoint"`
	Protocol string `yaml:"protocol"`
	Auth     string `yaml:"auth"`
	NoSchema bool   `yaml:"no_schema"`
}

// resolve validates one entry into a backend. Every failure here is a failure
// to start: a table that names a protocol dragoman cannot speak is not a table
// dragoman can serve from, and finding that out on the first request instead
// would be finding it out in production.
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
	// The variable is required; its value is not. An instance serving six
	// providers has to start with the one key its operator holds, so an unset
	// variable fails when the service is called rather than here. A service
	// dragoman spawns is the exception: it was signed in once, interactively,
	// and there is no variable to name.
	if entry.Auth == "" && !base.IsSpawned(entry.Endpoint) {
		return base.Backend{}, fmt.Errorf("config: service %q has no auth (name the environment variable holding the key)", name)
	}
	return base.Backend{
		Name:     name,
		Dialect:  entry.Protocol,
		Endpoint: entry.Endpoint,
		KeyEnv:   entry.Auth,
		NoSchema: entry.NoSchema,
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
