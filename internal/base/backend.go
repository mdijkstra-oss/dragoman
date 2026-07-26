package base

// Backend is a dialect at a URL with a key in the environment, plus the record
// of what that particular endpoint does differently. Sharing a wire shape does
// not make two endpoints identical, and those differences are data rather than
// code: a record in the preset table beside the endpoint and dialect.
type Backend struct {
	Name     string
	Dialect  string
	Endpoint string
	KeyEnv   string
	Quirks   Quirks
}

// Quirks is the per-backend record. An empty record is the property the
// abstraction protects: OpenRouter needs nothing here.
type Quirks struct {
	// RequiresMaxTokens marks a backend that will not accept a request
	// without an output cap.
	RequiresMaxTokens bool

	// MaxTokensFallback fires only when a backend requires a field that
	// neither flag nor body supplied. No other backend ever receives an
	// invented value, so absent stays absent everywhere it still can.
	MaxTokensFallback *int

	// MaxTokensField relocates the output cap. Two endpoints serving the same
	// wire shape can disagree about its name, and the one that is not read is
	// silently ignored rather than rejected.
	MaxTokensField string

	// StreamUsageOption marks a backend that reports usage while streaming
	// only when asked for it in the body.
	StreamUsageOption bool

	// NoStructuredSchema marks a backend with no schema mode. Since dragoman
	// does not write prompts, a schema against such a backend is dropped and
	// reported, and the caller gets prose.
	NoStructuredSchema bool

	// StreamUpstream marks a backend that must be streamed from even when the
	// caller asked for one buffered body, because folding that stream is
	// defined to produce exactly that body. Invisible to the caller either
	// way.
	StreamUpstream bool
}

// WithMaxTokens applies a backend's required-field fallback, returning the cap
// to send and whether one had to be invented.
func WithMaxTokens(quirks Quirks, requested *int) (*int, bool) {
	if requested != nil {
		return requested, false
	}
	if !quirks.RequiresMaxTokens {
		return nil, false
	}
	return quirks.MaxTokensFallback, quirks.MaxTokensFallback != nil
}
