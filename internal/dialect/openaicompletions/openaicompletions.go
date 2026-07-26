// Package openaicompletions speaks POST /v1/chat/completions — a body format
// several companies serve. It is the one decoder that has to infer item
// structure rather than read it: a new tool call is a new index appearing, and
// the turn ends when the stream says it is over.
package openaicompletions

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/matthijn/dragoman/internal/base"
	"github.com/matthijn/dragoman/internal/dialect"
)

// Name is the dialect's value on the command line.
const Name = "openai-completions"

// DefaultMaxTokensField is the cap field name most endpoints of this shape
// read. OpenAI's own endpoint wants max_completion_tokens instead, which is a
// quirks-record entry rather than a second dialect.
const DefaultMaxTokensField = "max_tokens"

// Keys under which the decoder tracks the items it infers.
const (
	reasoningKey = "reasoning"
	messageKey   = "message"
	callPrefix   = "call:"
)

// Dialect returns the wire format.
func Dialect() dialect.Dialect {
	return dialect.Dialect{
		Name:   Name,
		Path:   "/chat/completions",
		KeyEnv: "OPENAI_API_KEY",
		Headers: func(key string) map[string]string {
			return map[string]string{
				"Authorization": "Bearer " + key,
				"Content-Type":  "application/json",
			}
		},
		Encode:       encode,
		NewState:     func() dialect.State { return decodeState{} },
		StreamPieces: streamPieces,
		BodyPieces:   bodyPieces,
		DropTable: map[string]base.DropReason{
			"temperature":       base.DropUnsupported,
			"top_p":             base.DropUnsupported,
			"top_k":             base.DropUnsupported,
			"seed":              base.DropUnsupported,
			"frequency_penalty": base.DropUnsupported,
			"presence_penalty":  base.DropUnsupported,
			"logit_bias":        base.DropUnsupported,
			"logprobs":          base.DropUnsupported,
			"top_logprobs":      base.DropUnsupported,
			"n":                 base.DropUnsupported,
		},
	}
}

// carries lists the base fields this dialect places somewhere.
var carries = dialect.Carries{
	"model", "instructions", "input", "tools", "tool_choice",
	"text.format", "reasoning.effort", "max_output_tokens", "stream",
	"parallel_tool_calls",
}

func encode(req base.Request, backend base.Backend, stream bool) (dialect.Encoded, error) {
	drops, err := base.Unmodelled(req.Raw, req)
	if err != nil {
		return dialect.Encoded{}, fmt.Errorf("%s: %w", Name, err)
	}
	drops = append(drops, dialect.Unread(req, carries)...)

	messages, itemDrops, err := buildMessages(req)
	if err != nil {
		return dialect.Encoded{}, fmt.Errorf("%s: %w", Name, err)
	}
	drops = append(drops, itemDrops...)

	body := map[string]any{"messages": messages, "stream": stream}
	if model := base.GetModel(req); model != "" {
		body["model"] = model
	}
	if stream && backend.Quirks.StreamUsageOption {
		body["stream_options"] = map[string]any{"include_usage": true}
	}
	if cap, _ := base.WithMaxTokens(backend.Quirks, req.MaxOutputTokens); cap != nil {
		body[capField(backend)] = *cap
	}
	if effort := base.GetEffort(req); effort != "" {
		body["reasoning_effort"] = effort
	}
	if req.ParallelToolCalls != nil {
		body["parallel_tool_calls"] = *req.ParallelToolCalls
	}

	tools, toolDrops := dialect.FunctionTools(req.Tools)
	drops = append(drops, toolDrops...)
	if len(tools) > 0 {
		body["tools"] = buildTools(tools)
	}
	if choice := toolChoice(req); choice != nil {
		body["tool_choice"] = choice
	}

	if format := base.GetFormat(req); format != nil && format.Type == "json_schema" {
		if backend.Quirks.NoStructuredSchema {
			// dragoman does not write prompts, so the documented workaround of
			// naming the shape in the message is not available. The caller
			// gets prose, and is told.
			drops = append(drops, base.Drop{
				Path:   "text.format",
				Reason: base.DropUnsupported,
				Detail: fmt.Sprintf("%s has no schema mode", backend.Name),
			})
		} else {
			schema := map[string]any{"name": format.Name, "schema": format.Schema}
			if format.Name == "" {
				schema["name"] = "response"
			}
			if format.Strict != nil {
				schema["strict"] = *format.Strict
			}
			body["response_format"] = map[string]any{"type": "json_schema", "json_schema": schema}
		}
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return dialect.Encoded{}, fmt.Errorf("%s: encode: %w", Name, err)
	}
	return dialect.Encoded{Body: encoded, Drops: drops}, nil
}

func capField(backend base.Backend) string {
	if backend.Quirks.MaxTokensField != "" {
		return backend.Quirks.MaxTokensField
	}
	return DefaultMaxTokensField
}

// buildMessages flattens the base's item list into the role-tagged messages
// this dialect expects. Consecutive assistant-side items collapse into one
// message so that a call and the text preceding it stay in the same turn, and
// text accumulates rather than replacing what is already there.
func buildMessages(req base.Request) ([]wireMessage, []base.Drop, error) {
	messages := []wireMessage{}
	if req.Instructions != nil && *req.Instructions != "" {
		messages = append(messages, wireMessage{Role: base.RoleSystem, Content: text(*req.Instructions)})
	}
	assistant := func() *wireMessage {
		if len(messages) > 0 && messages[len(messages)-1].Role == base.RoleAssistant {
			return &messages[len(messages)-1]
		}
		messages = append(messages, wireMessage{Role: base.RoleAssistant})
		return &messages[len(messages)-1]
	}

	drops, err := dialect.WalkItems(req.Input, dialect.Handlers{
		Message: func(item base.Item) {
			role := item.Role
			if role == "" {
				role = base.RoleUser
			}
			if role != base.RoleAssistant {
				messages = append(messages, wireMessage{Role: role, Content: text(base.GetText(item))})
				return
			}
			turn := assistant()
			turn.Content = text(join(turn.Content, base.GetText(item)))
		},
		Reasoning: func(item base.Item) {
			// This dialect has no reasoning item of its own; the backends that
			// reason here supply their own field, and whatever they returned
			// goes back unaltered.
			if item.EncryptedContent == "" {
				return
			}
			turn := assistant()
			turn.ReasoningContent = joinText(turn.ReasoningContent, item.EncryptedContent)
		},
		Call: func(item base.Item) error {
			turn := assistant()
			turn.ToolCalls = append(turn.ToolCalls, wireToolCall{
				ID:       item.CallID,
				Type:     "function",
				Function: wireCallBody{Name: item.Name, Arguments: item.Arguments},
			})
			return nil
		},
		Output: func(item base.Item) {
			messages = append(messages, wireMessage{
				Role:       "tool",
				ToolCallID: item.CallID,
				Content:    text(item.Output),
			})
		},
	})
	return messages, drops, err
}

// join appends to a turn's existing text rather than replacing it, so a run of
// assistant items keeps every one of them.
func join(existing *content, addition string) string {
	return joinText(contentText(existing), addition)
}

func joinText(existing, addition string) string {
	if existing == "" {
		return addition
	}
	if addition == "" {
		return existing
	}
	return existing + "\n\n" + addition
}

func buildTools(tools []base.Tool) []wireTool {
	built := make([]wireTool, 0, len(tools))
	for _, tool := range tools {
		built = append(built, wireTool{
			Type: "function",
			Function: wireFunction{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  tool.Parameters,
				Strict:      tool.Strict,
			},
		})
	}
	return built
}

// toolChoice places the named form one level deeper than the base does. A
// shape the base does not model is reported rather than sent as a mode string
// the provider does not define.
func toolChoice(req base.Request) any {
	known := dialect.KnownChoice(req)
	if known == nil {
		return nil
	}
	if known.Name != "" {
		return map[string]any{
			"type":     "function",
			"function": map[string]any{"name": known.Name},
		}
	}
	if known.Mode == "" {
		return nil
	}
	return known.Mode
}

// decodeState carries what the implicit grammar forces a decoder to remember:
// whether the response identity has been announced, and which inferred items
// are open. Item boundaries are not announced here — a new tool call is a new
// index appearing, and the turn is closed only by the terminal chunk.
type decodeState struct {
	Started bool
}

func streamPieces(state dialect.State, frame base.Frame) (dialect.State, dialect.Recognised, error) {
	current, ok := state.(decodeState)
	if !ok {
		return state, dialect.Recognised{}, fmt.Errorf("%s: bad decoder state", Name)
	}
	if len(frame.Data) == 0 || string(frame.Data) == "[DONE]" {
		return current, dialect.Recognised{}, nil
	}
	var chunk wireChunk
	if err := json.Unmarshal(frame.Data, &chunk); err != nil {
		return current, dialect.Recognised{}, fmt.Errorf("%s: chunk: %w", Name, err)
	}

	if chunk.Error != nil {
		return current, dialect.Recognised{
			Pieces: []base.Piece{base.FailurePiece(chunk.Error.Type, chunk.Error.Message)},
		}, nil
	}

	built := []base.Piece{}
	if !current.Started && chunk.ID != "" {
		current.Started = true
		built = append(built, base.Start(chunk.ID, chunk.Model, chunk.Created))
	}
	// A chunk with no choices is not an error and not a turn boundary; it is
	// where some endpoints report usage.
	if chunk.Usage != nil {
		built = append(built, usagePiece(chunk.Usage))
	}
	if len(chunk.Choices) == 0 {
		return current, dialect.Recognised{Pieces: built}, nil
	}

	choice := chunk.Choices[0]
	if choice.Delta != nil {
		built = append(built, deltaPieces(*choice.Delta)...)
	}
	if choice.FinishReason != nil && *choice.FinishReason != "" {
		built = append(built, outcomePiece(*choice.FinishReason))
	}
	return current, dialect.Recognised{Pieces: built}, nil
}

func bodyPieces(body []byte) (dialect.Recognised, error) {
	var completion wireCompletion
	if err := json.Unmarshal(body, &completion); err != nil {
		return dialect.Recognised{}, fmt.Errorf("%s: %w", Name, err)
	}
	if completion.Error != nil {
		return dialect.Recognised{Pieces: []base.Piece{
			base.Start(completion.ID, completion.Model, completion.Created),
			base.FailurePiece(completion.Error.Type, completion.Error.Message),
		}}, nil
	}
	built := []base.Piece{base.Start(completion.ID, completion.Model, completion.Created)}
	drops := []base.Drop{}

	if len(completion.Choices) > 0 {
		choice := completion.Choices[0]
		if choice.Message != nil {
			built = append(built, messagePieces(*choice.Message)...)
			drops = append(drops, messageDrops(choice.RawMessage, *choice.Message)...)
		}
		if choice.FinishReason != nil {
			built = append(built, outcomePiece(*choice.FinishReason))
		}
	}
	if completion.Usage != nil {
		built = append(built, usagePiece(completion.Usage))
	}
	return dialect.Recognised{Pieces: built, Drops: drops}, nil
}

// messagePieces renders a whole buffered message, opening and closing each
// item explicitly because nothing arrived as a delta.
//
// A buffered body cannot express ordering: reasoning, content and tool calls
// are separate fields on one message rather than a sequence. The canonical
// order below is the one both endpoints of this shape produce on the wire, so
// folding a real stream agrees with decoding the body it corresponds to. A
// stream that genuinely interleaved them differently would keep its own order,
// because arrival order there is information the body does not carry.
func messagePieces(message wireMessage) []base.Piece {
	built := []base.Piece{}
	if message.ReasoningContent != "" {
		built = append(built,
			base.OpenPiece(reasoningKey, base.ItemReasoning),
			base.SummaryPiece(reasoningKey, message.ReasoningContent),
			base.StatePiece(reasoningKey, message.ReasoningContent),
			base.ClosePiece(reasoningKey))
	}
	if content := contentText(message.Content); content != "" {
		built = append(built,
			base.OpenPiece(messageKey, base.ItemMessage),
			base.TextPiece(messageKey, content),
			base.ClosePiece(messageKey))
	}
	for index, call := range message.ToolCalls {
		key := callKey(index)
		if call.Index != nil {
			key = callKey(*call.Index)
		}
		built = append(built,
			base.OpenPiece(key, base.ItemFunctionCall),
			base.CallPiece(key, call.ID, call.Function.Name),
			base.ArgsPiece(key, call.Function.Arguments),
			base.ClosePiece(key))
	}
	return built
}

// deltaPieces infers item structure from which delta key is populated. Empty
// and null content are structural filler on both endpoints of this shape and
// must not be treated as text.
func deltaPieces(delta wireDelta) []base.Piece {
	built := []base.Piece{}
	if delta.ReasoningContent != nil && *delta.ReasoningContent != "" {
		built = append(built,
			base.SummaryPiece(reasoningKey, *delta.ReasoningContent),
			base.StatePiece(reasoningKey, *delta.ReasoningContent))
	}
	if delta.Content != nil && *delta.Content != "" {
		built = append(built, base.TextPiece(messageKey, *delta.Content))
	}
	for _, fragment := range delta.ToolCalls {
		index := 0
		if fragment.Index != nil {
			index = *fragment.Index
		}
		key := callKey(index)
		// Identity arrives on the first fragment for an index and never
		// again; a decoder that waits for it on later fragments loses it.
		if fragment.ID != "" || fragment.Function.Name != "" {
			built = append(built, base.CallPiece(key, fragment.ID, fragment.Function.Name))
		}
		if fragment.Function.Arguments != "" {
			built = append(built, base.ArgsPiece(key, fragment.Function.Arguments))
		}
	}
	return built
}

// outcomePiece maps a finish reason onto a base status. A value the dialect
// does not define passes through as a completed response rather than crashing
// a decoder — that set belongs to the provider.
func outcomePiece(finishReason string) base.Piece {
	switch finishReason {
	case "length":
		return base.OutcomePiece(base.StatusIncomplete, &base.IncompleteDetails{Reason: "max_output_tokens"}, nil)
	case "content_filter":
		return base.OutcomePiece(base.StatusIncomplete, &base.IncompleteDetails{Reason: "content_filter"}, nil)
	}
	return base.OutcomePiece(base.StatusCompleted, nil, nil)
}

func usagePiece(usage *wireUsage) base.Piece {
	return base.UsagePiece(&base.Usage{
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
		TotalTokens:  usage.TotalTokens,
	})
}

func messageDrops(raw json.RawMessage, message wireMessage) []base.Drop {
	if len(raw) == 0 {
		return nil
	}
	drops, err := base.Diff(raw, message)
	if err != nil {
		return nil
	}
	out := make([]base.Drop, 0, len(drops))
	for _, drop := range drops {
		drop.Path = "choices[0].message." + drop.Path
		out = append(out, drop)
	}
	return out
}

func callKey(index int) string { return callPrefix + strconv.Itoa(index) }

func text(value string) *content {
	if value == "" {
		return nil
	}
	rendered := content(value)
	return &rendered
}

// content is a message body, which arrives as a string, as null, or as an
// array of parts depending on the endpoint.
type content string

// contentText reads a message body that may be absent. A null content leaves
// the pointer nil without ever reaching UnmarshalJSON, which is what every
// tool call on one of these endpoints looks like.
func contentText(body *content) string {
	if body == nil {
		return ""
	}
	return string(*body)
}

func (c *content) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*c = ""
		return nil
	}
	var plain string
	if err := json.Unmarshal(data, &plain); err == nil {
		*c = content(plain)
		return nil
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(data, &parts); err != nil {
		return err
	}
	var joined strings.Builder
	for _, part := range parts {
		joined.WriteString(part.Text)
	}
	*c = content(joined.String())
	return nil
}

func (c content) MarshalJSON() ([]byte, error) { return json.Marshal(string(c)) }

type wireMessage struct {
	Role             string         `json:"role"`
	Content          *content       `json:"content,omitempty"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []wireToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
}

type wireToolCall struct {
	Index    *int         `json:"index,omitempty"`
	ID       string       `json:"id,omitempty"`
	Type     string       `json:"type,omitempty"`
	Function wireCallBody `json:"function"`
}

type wireCallBody struct {
	Name string `json:"name,omitempty"`
	// Arguments is a JSON string in both directions. Re-serialising it would
	// change bytes the provider handed out, so it is carried verbatim.
	Arguments string `json:"arguments"`
}

type wireTool struct {
	Type     string       `json:"type"`
	Function wireFunction `json:"function"`
}

type wireFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type wireError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Code    string `json:"code"`
}

type wireCompletion struct {
	Error   *wireError   `json:"error"`
	ID      string       `json:"id"`
	Model   string       `json:"model"`
	Created int64        `json:"created"`
	Choices []wireChoice `json:"choices"`
	Usage   *wireUsage   `json:"usage"`
}

type wireChoice struct {
	Message      *wireMessage    `json:"message"`
	RawMessage   json.RawMessage `json:"-"`
	FinishReason *string         `json:"finish_reason"`
}

func (c *wireChoice) UnmarshalJSON(data []byte) error {
	type plain wireChoice
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var envelope struct {
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return err
	}
	*c = wireChoice(decoded)
	c.RawMessage = envelope.Message
	return nil
}

type wireChunk struct {
	Error   *wireError        `json:"error"`
	ID      string            `json:"id"`
	Model   string            `json:"model"`
	Created int64             `json:"created"`
	Choices []wireChunkChoice `json:"choices"`
	Usage   *wireUsage        `json:"usage"`
}

type wireChunkChoice struct {
	Delta        *wireDelta `json:"delta"`
	FinishReason *string    `json:"finish_reason"`
}

type wireDelta struct {
	Role             string         `json:"role"`
	Content          *string        `json:"content"`
	ReasoningContent *string        `json:"reasoning_content"`
	ToolCalls        []wireToolCall `json:"tool_calls"`
}

type wireUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}
