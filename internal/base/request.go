package base

import (
	"encoding/json"
	"fmt"
)

// On Anthropic's reasoning models the cap covers thinking and output together,
// so a small one truncates mid-thought.
const DefaultMaxOutputTokens = 20000

// Optional scalars are pointers because providers default differently, and
// inventing a level nobody asked for changes both cost and behaviour.
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

	PromptCacheKey     json.RawMessage     `json:"prompt_cache_key,omitempty"`
	PromptCacheOptions *PromptCacheOptions `json:"prompt_cache_options,omitempty"`

	Raw json.RawMessage `json:"-"`
}

// A Responses input arrives as a bare string or as an item list.
type Input []Item

// Responses keeps a tool's schema flat; other dialects nest it at their own
// depth.
type Tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// A tool choice is a mode string (auto, none, required) or a named function,
// and every dialect spells the named form differently.
type ToolChoice struct {
	Mode string
	Name string
	Raw  json.RawMessage
}

func (c ToolChoice) Known() bool {
	if c.Name != "" {
		return true
	}
	return c.Mode == ToolChoiceAuto || c.Mode == ToolChoiceNone || c.Mode == ToolChoiceRequired
}

type TextConfig struct {
	Format    *Format `json:"format,omitempty"`
	Verbosity *string `json:"verbosity,omitempty"`
}

// Responses nests strict here; the two OpenAI dialects disagree about its depth.
type Format struct {
	Type   string          `json:"type"`
	Name   string          `json:"name,omitempty"`
	Schema json.RawMessage `json:"schema,omitempty"`
	Strict *bool           `json:"strict,omitempty"`
}

type Reasoning struct {
	Effort  *string `json:"effort,omitempty"`
	Summary *string `json:"summary,omitempty"`
}

// The TTL is request-wide in Responses and per breakpoint on anthropic-messages,
// so one value fans out onto every marker placed there. Converting it would
// choose a price.
type PromptCacheOptions struct {
	TTL *string
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

func GetCacheTTL(req Request) *string {
	if req.PromptCacheOptions == nil {
		return nil
	}
	return req.PromptCacheOptions.TTL
}

// Some dialects carry strictness per request rather than per tool, and cannot
// express tools that disagree about it.
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

func TextItem(role, partType, text string) Item {
	return Item{Type: ItemMessage, Role: role, Content: []ContentPart{{Type: partType, Text: text}}}
}

// A hosted capability named in the tool list belongs to the provider.
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
		c.Name = ""
	}
	return nil
}

type namedChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

func (o PromptCacheOptions) MarshalJSON() ([]byte, error) {
	if len(o.Raw) > 0 {
		return o.Raw, nil
	}
	return json.Marshal(cacheOptions{TTL: o.TTL})
}

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
