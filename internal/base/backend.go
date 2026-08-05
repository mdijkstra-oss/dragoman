package base

import "strings"

const SchemeExec = "exec"

func SchemeOf(endpoint string) string {
	scheme, _, found := strings.Cut(endpoint, ":")
	if !found {
		return ""
	}
	return scheme
}

func IsSpawned(endpoint string) bool { return SchemeOf(endpoint) == SchemeExec }

type Backend struct {
	Name     string
	Dialect  string
	Endpoint string
	KeyEnv   string

	// NoSchema marks an endpoint with no structured-output mode. It is a
	// capability rather than a placement, so it cannot live in the protocol:
	// DeepSeek and OpenRouter serve the same wire format and disagree about it.
	NoSchema bool

	// An agent harness resolves its own tool calls and has no channel to hand
	// one back.
	NoTools bool

	NoOutputCap bool

	// Claude Code runs a turn per user message rather than reading a history.
	OneTurnPerRequest bool
}
