package base

import (
	"encoding/json"
	"fmt"
)

// Request is an openai-responses request body. Optional scalars are pointers so
// that absent stays absent: a level nobody asked for is never filled in, since
// providers default differently and inventing one silently changes both cost
// and behaviour.
type Request struct {
	Model             *string        `json:"model,omitempty"`
	Instructions      *string        `json:"instructions,omitempty"`
	Input             Input          `json:"input,omitempty"`
	Tools             []Tool         `json:"tools,omitempty"`
	ToolChoice        *ToolChoice    `json:"tool_choice,omitempty"`
	Text              *TextConfig    `json:"text,omitempty"`
	Reasoning         *Reasoning     `json:"reasoning,omitempty"`
	MaxOutputTokens   *int           `json:"max_output_tokens,omitempty"`
	Stream            *bool          `json:"stream,omitempty"`
	ParallelToolCalls *bool          `json:"parallel_tool_calls,omitempty"`
	Store             *bool          `json:"store,omitempty"`
	Include           []string       `json:"include,omitempty"`
	Metadata          map[string]any `json:"metadata,omitempty"`

	// PromptCacheKey is bytes rather than a string because its value is the
	// provider's to judge. A caller who sends a shape this base did not expect
	// is forwarded and refused by the endpoint, where typing it would make
	// dragoman refuse first and on no authority.
	PromptCacheKey     json.RawMessage     `json:"prompt_cache_key,omitempty"`
	PromptCacheOptions *PromptCacheOptions `json:"prompt_cache_options,omitempty"`

	// Raw is the body exactly as it arrived. The openai-responses backend
	// passes it through unchanged when no flag overrides it, which is what
	// makes that path a faithful pass-through, and the dropped-field diff is
	// computed against it.
	Raw json.RawMessage `json:"-"`
}

// Input is a Responses input, which is either a bare string or an item list.
// The base is the same format as the wire, so both forms have to arrive.
type Input []Item

// Tool is a function tool definition. Responses keeps the schema flat; other
// dialects nest it at their own depth. A tool whose type is not "function"
// names a capability dragoman does not carry.
type Tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// ToolChoice is either a mode string (auto, none, required) or a named
// function. Every dialect spells the named form differently.
type ToolChoice struct {
	Mode string
	Name string
	// Raw is the shape as it arrived. Responses has forms beyond a mode and a
	// named function; keeping the bytes lets the base round-trip them and lets
	// a dialect that cannot express one report it rather than invent a value.
	Raw json.RawMessage
}

// Known reports whether a tool choice is one of the forms every dialect can
// express: a mode, or a named function.
func (c ToolChoice) Known() bool {
	if c.Name != "" {
		return true
	}
	return c.Mode == ToolChoiceAuto || c.Mode == ToolChoiceNone || c.Mode == ToolChoiceRequired
}

// TextConfig carries the structured-output format.
type TextConfig struct {
	Format    *Format `json:"format,omitempty"`
	Verbosity *string `json:"verbosity,omitempty"`
}

// Format is a structured-output request. Responses nests strict here; the two
// OpenAI dialects disagree with each other on the depth.
type Format struct {
	Type   string          `json:"type"`
	Name   string          `json:"name,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
	Strict *bool           `json:"strict,omitempty"`
}

// Reasoning carries the reasoning level. The value is opaque and passes
// through byte for byte.
type Reasoning struct {
	Effort  *string `json:"effort,omitempty"`
	Summary *string `json:"summary,omitempty"`
}

// PromptCacheOptions is the request-wide caching setting. TTL is named because
// it is request-wide here and per breakpoint on the dialect that spells a
// breakpoint its own way, so the one value fans out onto every marker it
// places. The value is opaque: a 30m is forwarded, never converted, because
// choosing which TTL a request becomes is choosing a price.
type PromptCacheOptions struct {
	TTL *string
	// Raw is the shape as it arrived. The dialects spelling this field OpenAI's
	// way forward the bytes, so an option OpenAI adds travels without a release
	// here; the one that reads the TTL out reports whatever else was inside.
	Raw json.RawMessage
}

const (
	ToolChoiceAuto     = "auto"
	ToolChoiceNone     = "none"
	ToolChoiceRequired = "required"
	ToolChoiceFunction = "function"
)

func ParseRequest(body []byte) (Request, error) {
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		return Request{}, fmt.Errorf("parse request: %w", err)
	}
	req.Raw = append(json.RawMessage(nil), body...)
	return req, nil
}

func GetModel(req Request) string {
	if req.Model == nil {
		return ""
	}
	return *req.Model
}

// GetEffort returns the reasoning level the request carries, or the empty
// string. The value is never normalised, case-folded, or validated.
func GetEffort(req Request) string {
	if req.Reasoning == nil || req.Reasoning.Effort == nil {
		return ""
	}
	return *req.Reasoning.Effort
}

func GetStream(req Request) bool { return req.Stream != nil && *req.Stream }

func GetFormat(req Request) *Format {
	if req.Text == nil {
		return nil
	}
	return req.Text.Format
}

// GetCacheTTL returns the request-wide cache TTL, or nil. Absent stays absent:
// a breakpoint with no TTL gets the backend's own default, and spelling one out
// where none arrived would choose a price nobody asked for.
func GetCacheTTL(req Request) *string {
	if req.PromptCacheOptions == nil {
		return nil
	}
	return req.PromptCacheOptions.TTL
}

// HasMixedStrictness reports whether the tools disagree about strictness,
// which a per-request strictness dialect cannot express.
func HasMixedStrictness(tools []Tool) bool {
	seenStrict, seenLoose := false, false
	for _, tool := range tools {
		if tool.Strict != nil && *tool.Strict {
			seenStrict = true
			continue
		}
		seenLoose = true
	}
	return seenStrict && seenLoose
}

func (i *Input) UnmarshalJSON(data []byte) error {
	var prompt string
	if err := json.Unmarshal(data, &prompt); err == nil {
		*i = Input{TextItem(RoleUser, PartInputText, prompt)}
		return nil
	}
	var items []Item
	if err := json.Unmarshal(data, &items); err != nil {
		return fmt.Errorf("input: %w", err)
	}
	*i = items
	return nil
}

// TextItem builds a single-part message item.
func TextItem(role, partType, text string) Item {
	return Item{Type: ItemMessage, Role: role, Content: []ContentPart{{Type: partType, Text: text}}}
}

// IsFunctionTool reports whether a tool is the one kind every dialect carries.
// A hosted capability named in the tool list belongs to the provider, not to
// dragoman, and is reported rather than encoded as a nameless function.
func IsFunctionTool(tool Tool) bool { return tool.Type == "" || tool.Type == "function" }

func (c ToolChoice) MarshalJSON() ([]byte, error) {
	if len(c.Raw) > 0 && !c.Known() {
		return c.Raw, nil
	}
	if c.Name != "" {
		return json.Marshal(namedChoice{Type: ToolChoiceFunction, Name: c.Name})
	}
	return json.Marshal(c.Mode)
}

func (c *ToolChoice) UnmarshalJSON(data []byte) error {
	raw := append(json.RawMessage(nil), data...)
	var mode string
	if err := json.Unmarshal(data, &mode); err == nil {
		*c = ToolChoice{Mode: mode, Raw: raw}
		return nil
	}
	var named namedChoice
	if err := json.Unmarshal(data, &named); err != nil {
		return fmt.Errorf("tool_choice: %w", err)
	}
	*c = ToolChoice{Mode: named.Type, Name: named.Name, Raw: raw}
	if named.Type != ToolChoiceFunction {
		// A shape the base does not model keeps its bytes and loses its
		// name, so a dialect asks Known() rather than guessing at Mode.
		c.Name = ""
	}
	return nil
}

type namedChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// MarshalJSON returns the bytes that arrived, so the drop diff sees the whole
// object consumed and a forwarding dialect sends it verbatim. A value dragoman
// built rather than parsed carries the TTL alone.
func (o PromptCacheOptions) MarshalJSON() ([]byte, error) {
	if len(o.Raw) > 0 {
		return o.Raw, nil
	}
	return json.Marshal(cacheOptions{TTL: o.TTL})
}

// UnmarshalJSON keeps the bytes whatever arrived in them. A shape the base does
// not model loses its TTL and nothing else, exactly as ToolChoice loses its
// name: the forwarding dialects still send what the caller wrote, and the one
// that reads a TTL out reports what it could not carry. Failing here instead
// would end the request before any provider saw it — dragoman refusing on a
// provider's behalf, for a value set it does not own, and on the pass-through
// path it would reject a body that used to travel byte for byte.
func (o *PromptCacheOptions) UnmarshalJSON(data []byte) error {
	raw := append(json.RawMessage(nil), data...)
	var decoded cacheOptions
	if err := json.Unmarshal(data, &decoded); err != nil {
		*o = PromptCacheOptions{Raw: raw}
		return nil
	}
	*o = PromptCacheOptions{TTL: decoded.TTL, Raw: raw}
	return nil
}

type cacheOptions struct {
	TTL *string `json:"ttl,omitempty"`
}
