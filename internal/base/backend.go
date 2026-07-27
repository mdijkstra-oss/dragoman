package base

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
}
