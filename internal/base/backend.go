package base

import "strings"

// SchemeExec names a backend reached by spawning a locally installed binary
// rather than by posting to a URL. The scheme is what selects the executor, so
// "https" and "exec" are the same kind of fact: where the backend is.
const SchemeExec = "exec"

// SchemeOf reports the scheme an endpoint names: "https" for a URL, "exec" for
// a locally installed binary.
func SchemeOf(endpoint string) string {
	scheme, _, found := strings.Cut(endpoint, ":")
	if !found {
		return ""
	}
	return scheme
}

// IsSpawned reports whether reaching a backend means starting a process. Such a
// backend authenticates itself and cannot be sent tools, an output cap or a
// second turn, so the scheme rather than the table decides all four.
func IsSpawned(endpoint string) bool { return SchemeOf(endpoint) == SchemeExec }

// Backend is a protocol at a URL with a key in the environment. Nothing here
// says where a field goes on the wire: placement belongs to the protocol, which
// is the only thing that knows its own shape. What remains is what an endpoint
// knows about itself and no protocol could.
type Backend struct {
	Name     string
	Dialect  string
	Endpoint string
	KeyEnv   string

	// NoSchema marks an endpoint with no structured-output mode. It is a
	// capability rather than a placement, so it cannot live in the protocol:
	// DeepSeek and OpenRouter serve the same wire format and disagree about it.
	NoSchema bool

	// NoTools marks an endpoint with no channel for caller-declared tools. An
	// agent harness resolves tool calls itself and cannot hand one back, so
	// tools against it are dropped and reported rather than sent into a void.
	NoTools bool

	// NoOutputCap marks an endpoint with nowhere to put an output cap, so a cap
	// the caller supplied is reported rather than silently ignored.
	NoOutputCap bool

	// OneTurnPerRequest marks an endpoint that executes a turn per user message
	// rather than reading a whole history. A second user message there is not a
	// dropped field but a different conversation, so it is refused.
	OneTurnPerRequest bool
}
