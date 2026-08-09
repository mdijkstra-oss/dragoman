package base

import "strings"

func SchemeOf(endpoint string) string {
	scheme, _, found := strings.Cut(endpoint, ":")
	if !found {
		return ""
	}
	return scheme
}

type Backend struct {
	Name     string
	Dialect  string
	Endpoint string
	KeyEnv   string

	// DeepSeek's JSON mode takes no schema and asks for the shape in the prompt.
	// DeepSeek and OpenRouter serve the same wire format and disagree about this.
	SchemaAsPrompt bool

	// Which field carries the output cap, where an endpoint does not take the
	// default. Empty is the default spelling.
	MaxOutputTokensAs string
}
