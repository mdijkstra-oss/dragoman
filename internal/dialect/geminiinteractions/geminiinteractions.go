// Package geminiinteractions speaks POST /v1beta/interactions. Its input is a
// homogeneous step list rather than a turn list, and a response's steps splice
// back into the next request's input verbatim — which is the whole return leg.
package geminiinteractions

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/mdijkstra-oss/dragoman/internal/base"
	"github.com/mdijkstra-oss/dragoman/internal/dialect"
)

const Name = "gemini-interactions"

// Pins the request schema so a future default cannot move the shape under the
// decoder. Free to send.
const Revision = "2026-05-20"

const (
	stepUserInput      = "user_input"
	stepModelOutput    = "model_output"
	stepThought        = "thought"
	stepFunctionCall   = "function_call"
	stepFunctionResult = "function_result"
)

func Dialect() dialect.Dialect {
	return dialect.Dialect{
		Name:   Name,
		Path:   "/interactions",
		KeyEnv: "GEMINI_API_KEY",
		Headers: func(key string) map[string]string {
			return map[string]string{
				"x-goog-api-key": key,
				"Api-Revision":   Revision,
				"Content-Type":   "application/json",
			}
		},
		Encode:       encode,
		NewState:     func() dialect.State { return decodeState{} },
		StreamPieces: streamPieces,
		BodyPieces:   bodyPieces,
		DropTable: dialect.DropTable(map[string]base.DropReason{
			"strict": base.DropUnsupported,
			// There is no inline caching field here. Both routes that would
			// stand in for one are stateful — CachedContent on the legacy API,
			// or a previous interaction id — and a stateless proxy reaches
			// neither.
			"prompt_cache_key":     base.DropUnsupported,
			"prompt_cache_options": base.DropUnsupported,
		}),
	}
}

var placedFields = dialect.Carries{
	"model", "instructions", "input", "tools", "tool_choice",
	"text.format", "reasoning.effort", "max_output_tokens", "stream",
}

func encode(req base.Request, backend base.Backend, stream bool) (dialect.Encoded, error) {
	drops, err := base.Unmodelled(req.Raw, req)
	if err != nil {
		return dialect.Encoded{}, fmt.Errorf("%s: %w", Name, err)
	}
	drops = append(drops, dialect.Unread(req, placedFields)...)

	steps, system, itemDrops, err := buildSteps(req)
	if err != nil {
		return dialect.Encoded{}, fmt.Errorf("%s: %w", Name, err)
	}
	drops = append(drops, itemDrops...)
	drops = append(drops, markerDrops(req.Input)...)

	body := wireRequest{Model: base.GetModel(req), Input: steps, Stream: stream}
	if system != "" {
		body.SystemInstruction = system
	}

	tools, toolDrops := dialect.FunctionTools(req.Tools)
	drops = append(drops, toolDrops...)
	for _, tool := range tools {
		body.Tools = append(body.Tools, wireTool{
			Type:        "function",
			Name:        tool.Name,
			Description: tool.Description,
			Parameters:  tool.Parameters,
		})
	}

	config := wireGenerationConfig{}
	if cap := req.MaxOutputTokens; cap != nil {
		config.MaxOutputTokens = cap
	}
	if effort := base.GetEffort(req); effort != "" {
		config.ThinkingLevel = effort
	}

	choice, choiceDrops := toolChoice(req)
	config.ToolChoice = choice
	drops = append(drops, choiceDrops...)

	if format := base.GetFormat(req); format != nil && format.Type == "json_schema" {
		body.ResponseFormat = &wireResponseFormat{
			Type:     "text",
			MimeType: "application/json",
			Schema:   format.Schema,
		}
		if format.Strict != nil {
			drops = append(drops, strictness(*format.Strict))
		}
		if format.Name != "" {
			drops = append(drops, base.Drop{Path: "text.format.name", Reason: base.DropInert,
				Detail: Name + " has no name on a response format, and a label on a schema changes no answer"})
		}
	}
	if config != (wireGenerationConfig{}) {
		body.GenerationConfig = &config
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return dialect.Encoded{}, fmt.Errorf("%s: encode: %w", Name, err)
	}
	return dialect.Encoded{Body: encoded, Drops: drops}, nil
}

// A response schema here constrains decoding whether or not it was asked to, so
// only asking to relax it is lost.
func strictness(strict bool) base.Drop {
	if strict {
		return base.Drop{Path: "text.format.strict", Reason: base.DropInert,
			Detail: Name + " has no strictness flag: a response schema is always enforced"}
	}
	return base.Drop{Path: "text.format.strict", Reason: base.DropUnsupported,
		Detail: Name + " has no strictness flag: the schema is enforced though the request asked it not to be"}
}

// A function result must name the function it answers, which the base carries
// only on the call.
func buildSteps(req base.Request) ([]wireStep, string, []base.Drop, error) {
	system := ""
	if req.Instructions != nil {
		system = *req.Instructions
	}
	names := callNames(req.Input)
	steps := make([]wireStep, 0, len(req.Input))

	drops, err := dialect.WalkItems(req.Input, dialect.Handlers{
		Message: func(item base.Item) {
			text := base.GetText(item)
			if item.Role == base.RoleSystem || item.Role == base.RoleDeveloper {
				system = dialect.JoinText(system, text)
				return
			}
			stepType := stepUserInput
			if item.Role == base.RoleAssistant {
				stepType = stepModelOutput
			}
			steps = append(steps, wireStep{
				Type:    stepType,
				Content: []wireContent{{Type: "text", Text: text}},
			})
		},
		Reasoning: func(item base.Item) {
			if item.EncryptedContent == "" {
				return
			}
			steps = append(steps, wireStep{Type: stepThought, Signature: item.EncryptedContent})
		},
		Call: func(item base.Item) error {
			arguments := json.RawMessage(item.Arguments)
			if len(arguments) == 0 {
				arguments = json.RawMessage("{}")
			}
			if !json.Valid(arguments) {
				return fmt.Errorf("function_call %q: arguments are not valid JSON", item.Name)
			}
			// The one dialect carrying a decoded object on a buffered call.
			steps = append(steps, wireStep{
				Type:      stepFunctionCall,
				ID:        item.CallID,
				Signature: item.EncryptedContent,
				Name:      item.Name,
				Arguments: arguments,
			})
			return nil
		},
		Output: func(item base.Item) {
			// JSON and Go literals disagree on control characters, and a Go
			// literal here would make the whole request unencodable.
			result, err := json.Marshal(item.Output)
			if err != nil {
				result = []byte(`""`)
			}
			steps = append(steps, wireStep{
				Type:   stepFunctionResult,
				CallID: item.CallID,
				Name:   names[item.CallID],
				Result: result,
			})
		},
	})
	return steps, system, drops, err
}

func markerDrops(items []base.Item) []base.Drop {
	drops := []base.Drop{}
	for index, item := range items {
		for position, part := range item.Content {
			if !part.Marks() {
				continue
			}
			drops = append(drops, base.Drop{
				Path:   dialect.MarkerPath(index, position),
				Reason: base.DropUnsupported,
				Detail: Name + " has no cache breakpoint, and both of its caching routes are stateful",
			})
		}
	}
	return drops
}

func callNames(items []base.Item) map[string]string {
	names := map[string]string{}
	for _, item := range items {
		if item.Type == base.ItemFunctionCall {
			names[item.CallID] = item.Name
		}
	}
	return names
}

// Strictness is per request here rather than per tool, so tools that disagree
// about it cannot be expressed.
func toolChoice(req base.Request) (any, []base.Drop) {
	drops := []base.Drop{}
	if base.HasMixedStrictness(req.Tools) {
		drops = append(drops, base.Drop{
			Path:   "tools[].strict",
			Reason: base.DropUnsupported,
			Detail: Name + " carries strictness per request, not per tool; a mix cannot be expressed",
		})
	}
	known := dialect.KnownChoice(req)
	if known != nil && known.Name != "" {
		return wireToolChoice{Allowed: wireAllowed{Mode: "any", Tools: []string{known.Name}}}, drops
	}
	// Strictness is a routing mode here: validated is auto plus constrained
	// decoding.
	mode := ""
	if known != nil {
		mode = dialect.ChoiceModes[known.Mode]
	}
	if allStrict(req.Tools) && (mode == "" || mode == "auto") {
		return "validated", drops
	}
	if mode == "" {
		return nil, drops
	}
	return mode, drops
}

func allStrict(tools []base.Tool) bool {
	if len(tools) == 0 {
		return false
	}
	for _, tool := range tools {
		if tool.Strict == nil || !*tool.Strict {
			return false
		}
	}
	return true
}

// Boundaries are explicit and step-scoped here, so nothing has to be inferred.
type decodeState struct {
	Started bool
}

func streamPieces(state dialect.State, frame base.Frame) (dialect.State, dialect.DecodedFrame, error) {
	current, ok := state.(decodeState)
	if !ok {
		return state, dialect.DecodedFrame{}, fmt.Errorf("%s: bad decoder state", Name)
	}
	if len(frame.Data) == 0 || string(frame.Data) == "[DONE]" {
		return current, dialect.DecodedFrame{}, nil
	}
	var event wireEvent
	if err := json.Unmarshal(frame.Data, &event); err != nil {
		return current, dialect.DecodedFrame{}, fmt.Errorf("%s: event: %w", Name, err)
	}
	if event.Error != nil {
		return current, dialect.DecodedFrame{
			Pieces: []base.Piece{dialect.Failure(event.Error)},
		}, nil
	}
	name := event.EventType
	if name == "" {
		name = frame.Event
	}

	switch name {
	case "interaction.created":
		if event.Interaction == nil || current.Started {
			return current, dialect.DecodedFrame{}, nil
		}
		current.Started = true
		return current, dialect.DecodedFrame{Pieces: []base.Piece{
			base.Start(event.Interaction.ID, event.Interaction.Model, createdAt(event.Interaction.Created)),
		}}, nil

	case "step.start":
		if event.Step == nil {
			return current, dialect.DecodedFrame{}, nil
		}
		return current, dialect.DecodedFrame{Pieces: openPieces(event.Index, *event.Step)}, nil

	case "step.delta":
		if event.Delta == nil {
			return current, dialect.DecodedFrame{}, nil
		}
		return current, dialect.DecodedFrame{Pieces: deltaPieces(event.Index, *event.Delta)}, nil

	case "step.stop":
		return current, dialect.DecodedFrame{Pieces: []base.Piece{base.ClosePiece(dialect.Key(event.Index))}}, nil

	case "interaction.completed":
		if event.Interaction == nil {
			return current, dialect.DecodedFrame{}, nil
		}
		return current, dialect.DecodedFrame{Pieces: outcomePieces(*event.Interaction)}, nil
	}
	return current, dialect.DecodedFrame{}, nil
}

func bodyPieces(body []byte) (dialect.DecodedFrame, error) {
	var interaction wireInteraction
	if err := json.Unmarshal(body, &interaction); err != nil {
		return dialect.DecodedFrame{}, fmt.Errorf("%s: %w", Name, err)
	}
	if interaction.Error != nil {
		return dialect.DecodedFrame{Pieces: []base.Piece{
			base.Start(interaction.ID, interaction.Model, 0),
			dialect.Failure(interaction.Error),
		}}, nil
	}
	pieces := []base.Piece{base.Start(interaction.ID, interaction.Model, createdAt(interaction.Created))}
	for index, step := range interaction.Steps {
		pieces = append(pieces, openPieces(index, step)...)
		pieces = append(pieces, contentPieces(index, step)...)
		pieces = append(pieces, base.ClosePiece(dialect.Key(index)))
	}
	pieces = append(pieces, outcomePieces(interaction)...)

	drops := []base.Drop{}
	for index, raw := range interaction.RawSteps {
		if index >= len(interaction.Steps) {
			continue
		}
		drops = append(drops, dialect.ShapeDrops(raw, interaction.Steps[index], dialect.IndexedPath("steps", index))...)
	}
	return dialect.DecodedFrame{Pieces: pieces, Drops: drops}, nil
}

// A call's signature arrives in full rather than as a delta when the state
// belongs to the call itself.
func openPieces(index int, step wireStep) []base.Piece {
	kind := kindOf(step.Type)
	if kind == "" {
		return nil
	}
	pieces := []base.Piece{base.OpenPiece(dialect.Key(index), kind)}
	if step.Type == stepFunctionCall {
		pieces = append(pieces, base.CallPiece(dialect.Key(index), step.ID, step.Name))
	}
	if step.Signature != "" {
		pieces = append(pieces, base.StatePiece(dialect.Key(index), step.Signature))
	}
	return pieces
}

func contentPieces(index int, step wireStep) []base.Piece {
	switch step.Type {
	case stepModelOutput:
		var text strings.Builder
		for _, part := range step.Content {
			text.WriteString(part.Text)
		}
		if text.Len() == 0 {
			return nil
		}
		return []base.Piece{base.TextPiece(dialect.Key(index), text.String())}
	case stepFunctionCall:
		if len(step.Arguments) == 0 {
			return nil
		}
		return []base.Piece{base.ArgsPiece(dialect.Key(index), string(step.Arguments))}
	}
	return nil
}

func deltaPieces(index int, delta wireDelta) []base.Piece {
	switch delta.Type {
	case "text":
		return []base.Piece{base.TextPiece(dialect.Key(index), delta.Text)}
	case "thought_signature":
		return []base.Piece{base.StatePiece(dialect.Key(index), delta.Signature)}
	case "arguments_delta":
		return []base.Piece{base.ArgsPiece(dialect.Key(index), delta.Arguments)}
	}
	return nil
}

// The terminal event is always interaction.completed and the outcome lives in
// the payload; there are no distinct terminal event names.
func outcomePieces(interaction wireInteraction) []base.Piece {
	pieces := []base.Piece{}
	if usage := interaction.Usage; usage != nil {
		// The answer and the thinking arrive as siblings here and Responses
		// counts thinking inside the output, so they are added. This API reports
		// no cache write count, only what a read saved.
		pieces = append(pieces, base.UsagePiece(base.CountUsage(
			usage.TotalInputTokens,
			usage.TotalOutputTokens+usage.TotalThoughtTokens,
			usage.TotalThoughtTokens,
			usage.TotalCachedTokens, 0)))
	}
	return append(pieces, dialect.Outcome(interaction.Status, terminalStatuses))
}

var terminalStatuses = map[string]string{"incomplete": "max_output_tokens"}

func kindOf(stepType string) base.ItemType { return stepKinds[stepType] }

var stepKinds = map[string]base.ItemType{
	stepThought:      base.ItemReasoning,
	stepModelOutput:  base.ItemMessage,
	stepFunctionCall: base.ItemFunctionCall,
}

func createdAt(stamp string) int64 {
	if stamp == "" {
		return 0
	}
	parsed, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return 0
	}
	return parsed.Unix()
}

type wireRequest struct {
	Model             string                `json:"model,omitempty"`
	Input             []wireStep            `json:"input"`
	SystemInstruction string                `json:"system_instruction,omitempty"`
	Tools             []wireTool            `json:"tools,omitempty"`
	ResponseFormat    *wireResponseFormat   `json:"response_format,omitempty"`
	GenerationConfig  *wireGenerationConfig `json:"generation_config,omitempty"`
	Stream            bool                  `json:"stream"`
}

type wireStep struct {
	Type      string          `json:"type"`
	ID        string          `json:"id,omitempty"`
	Signature string          `json:"signature,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Result    json.RawMessage `json:"result,omitempty"`
	Content   []wireContent   `json:"content,omitempty"`
}

type wireContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type wireTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

type wireResponseFormat struct {
	Type     string          `json:"type"`
	MimeType string          `json:"mime_type"`
	Schema   json.RawMessage `json:"schema,omitempty"`
}

type wireGenerationConfig struct {
	MaxOutputTokens *int   `json:"max_output_tokens,omitempty"`
	ThinkingLevel   string `json:"thinking_level,omitempty"`
	ToolChoice      any    `json:"tool_choice,omitempty"`
}

type wireToolChoice struct {
	Allowed wireAllowed `json:"allowed_tools"`
}

type wireAllowed struct {
	Mode  string   `json:"mode"`
	Tools []string `json:"tools"`
}

type wireInteraction struct {
	Error    *dialect.WireError `json:"error"`
	ID       string             `json:"id"`
	Model    string             `json:"model"`
	Status   string             `json:"status"`
	Created  string             `json:"created"`
	Steps    []wireStep         `json:"steps"`
	RawSteps []json.RawMessage  `json:"-"`
	Usage    *wireUsage         `json:"usage"`
}

func (i *wireInteraction) UnmarshalJSON(data []byte) error {
	type plain wireInteraction
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*i = wireInteraction(decoded)
	i.RawSteps = dialect.RawElements(data, "steps")
	return nil
}

type wireUsage struct {
	TotalTokens        int `json:"total_tokens"`
	TotalInputTokens   int `json:"total_input_tokens"`
	TotalOutputTokens  int `json:"total_output_tokens"`
	TotalThoughtTokens int `json:"total_thought_tokens"`
	TotalCachedTokens  int `json:"total_cached_tokens"`
}

type wireEvent struct {
	Error       *dialect.WireError `json:"error"`
	EventType   string             `json:"event_type"`
	Index       int                `json:"index"`
	Step        *wireStep          `json:"step"`
	Delta       *wireDelta         `json:"delta"`
	Interaction *wireInteraction   `json:"interaction"`
}

type wireDelta struct {
	Type      string `json:"type"`
	Text      string `json:"text"`
	Signature string `json:"signature"`
	Arguments string `json:"arguments"`
}
