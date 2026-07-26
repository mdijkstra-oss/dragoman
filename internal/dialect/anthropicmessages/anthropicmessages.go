// Package anthropicmessages speaks POST /v1/messages. It is the only backend
// that requires an output cap, and the only one that nests a tool result
// inside a user message rather than making it a sibling item.
package anthropicmessages

import (
	"encoding/json"
	"fmt"

	"github.com/matthijn/dragoman/internal/base"
	"github.com/matthijn/dragoman/internal/dialect"
)

// Name is the dialect's value on the command line.
const Name = "anthropic-messages"

// Version is the API version header the endpoint requires; without it the
// request is rejected before it is read.
const Version = "2023-06-01"

func Dialect() dialect.Dialect {
	return dialect.Dialect{
		Name:   Name,
		Path:   "/messages",
		KeyEnv: "ANTHROPIC_API_KEY",
		Headers: func(key string) map[string]string {
			return map[string]string{
				"x-api-key":         key,
				"anthropic-version": Version,
				"Content-Type":      "application/json",
			}
		},
		Encode:       encode,
		NewState:     func() dialect.State { return decodeState{} },
		StreamPieces: streamPieces,
		BodyPieces:   bodyPieces,
		DropTable: dialect.DropTable(map[string]base.DropReason{
			"strict": base.DropUnsupported,
			"name":   base.DropUnsupported,
		}),
	}
}

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

	cap, _ := base.WithMaxTokens(backend.Quirks, req.MaxOutputTokens)
	if cap == nil {
		return dialect.Encoded{}, fmt.Errorf(
			"%s requires an output cap and none was supplied: pass --max-tokens or set it in the body (field max_tokens)", Name)
	}

	body := wireRequest{Model: base.GetModel(req), MaxTokens: *cap, Stream: stream}

	system, messages, itemDrops, err := buildMessages(req)
	if err != nil {
		return dialect.Encoded{}, fmt.Errorf("%s: %w", Name, err)
	}
	body.System, body.Messages = system, messages
	drops = append(drops, itemDrops...)

	tools, toolDrops := dialect.FunctionTools(req.Tools)
	drops = append(drops, toolDrops...)
	for _, tool := range tools {
		body.Tools = append(body.Tools, wireTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.Parameters,
			Strict:      tool.Strict,
		})
	}

	body.ToolChoice = toolChoice(req)
	if effort := base.GetEffort(req); effort != "" {
		body.OutputConfig = &wireOutputConfig{Effort: effort}
	}
	if format := base.GetFormat(req); format != nil && format.Type == "json_schema" {
		if body.OutputConfig == nil {
			body.OutputConfig = &wireOutputConfig{}
		}
		body.OutputConfig.Format = &wireFormat{Type: format.Type, Schema: format.Schema}
		if format.Name != "" {
			drops = append(drops, base.Drop{Path: "text.format.name", Reason: base.DropUnsupported,
				Detail: Name + " has no name on a structured-output format"})
		}
		if format.Strict != nil {
			drops = append(drops, base.Drop{Path: "text.format.strict", Reason: base.DropUnsupported,
				Detail: Name + " carries strictness per tool, not per format"})
		}
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return dialect.Encoded{}, fmt.Errorf("%s: encode: %w", Name, err)
	}
	return dialect.Encoded{Body: encoded, Drops: drops}, nil
}

// buildMessages groups the base's flat item list into this dialect's turns. A
// tool result must sit in the user message that immediately follows the
// assistant message holding its call, so consecutive items of the same side
// collapse into one turn; an input that separates them is the caller's
// ordering and the backend rejects it in its own words.
func buildMessages(req base.Request) (string, []wireMessage, []base.Drop, error) {
	system := ""
	if req.Instructions != nil {
		system = *req.Instructions
	}
	messages := []wireMessage{}
	appendBlock := func(role string, block wireBlock) {
		if len(messages) > 0 && messages[len(messages)-1].Role == role {
			messages[len(messages)-1].Content = append(messages[len(messages)-1].Content, block)
			return
		}
		messages = append(messages, wireMessage{Role: role, Content: []wireBlock{block}})
	}

	drops, err := dialect.WalkItems(req.Input, dialect.Handlers{
		Message: func(item base.Item) {
			text := base.GetText(item)
			if item.Role == base.RoleSystem || item.Role == base.RoleDeveloper {
				system = dialect.JoinText(system, text)
				return
			}
			role := base.RoleUser
			if item.Role == base.RoleAssistant {
				role = base.RoleAssistant
			}
			appendBlock(role, wireBlock{Type: "text", Text: text})
		},
		Reasoning: func(item base.Item) {
			// Whatever the backend returned as state goes back unaltered and
			// in position: the signature is byte-identical or the request is
			// rejected.
			if item.EncryptedContent == "" {
				return
			}
			appendBlock(base.RoleAssistant, wireBlock{
				Type:      "thinking",
				Thinking:  base.GetSummary(item),
				Signature: item.EncryptedContent,
			})
		},
		Call: func(item base.Item) error {
			input := json.RawMessage(item.Arguments)
			if len(input) == 0 {
				input = json.RawMessage("{}")
			}
			if !json.Valid(input) {
				return fmt.Errorf("function_call %q: arguments are not valid JSON", item.Name)
			}
			appendBlock(base.RoleAssistant, wireBlock{
				Type: "tool_use", ID: item.CallID, Name: item.Name, Input: input,
			})
			return nil
		},
		Output: func(item base.Item) {
			appendBlock(base.RoleUser, wireBlock{
				Type: "tool_result", ToolUseID: item.CallID, Content: item.Output,
			})
		},
	})
	return system, messages, drops, err
}

// toolChoice maps the base's choice, and carries the parallel-call setting
// that this dialect keeps on the same object. A request that only disables
// parallel calls still needs a choice to hang it from.
func toolChoice(req base.Request) *wireToolChoice {
	choice := (*wireToolChoice)(nil)
	if known := dialect.KnownChoice(req); known != nil {
		if known.Name != "" {
			choice = &wireToolChoice{Type: "tool", Name: known.Name}
		} else if mapped, isMode := dialect.ChoiceModes[known.Mode]; isMode {
			choice = &wireToolChoice{Type: mapped}
		}
	}
	if req.ParallelToolCalls != nil && !*req.ParallelToolCalls {
		if choice == nil {
			choice = &wireToolChoice{Type: "auto"}
		}
		disable := true
		choice.DisableParallelToolUse = &disable
	}
	return choice
}

// decodeState holds what is bounded by one message: the input token count seen
// at message_start, which the terminal frame has to be combined with because
// the two halves of usage arrive on different frames.
type decodeState struct {
	InputTokens int
}

func streamPieces(state dialect.State, frame base.Frame) (dialect.State, dialect.Recognised, error) {
	current, ok := state.(decodeState)
	if !ok {
		return state, dialect.Recognised{}, fmt.Errorf("%s: bad decoder state", Name)
	}
	if len(frame.Data) == 0 {
		return current, dialect.Recognised{}, nil
	}
	var event wireEvent
	if err := json.Unmarshal(frame.Data, &event); err != nil {
		return current, dialect.Recognised{}, fmt.Errorf("%s: event: %w", Name, err)
	}

	switch event.Type {
	case "error":
		if event.Error == nil {
			return current, dialect.Recognised{}, nil
		}
		return current, dialect.Recognised{
			Pieces: []base.Piece{dialect.Failure(event.Error)},
		}, nil

	case "message_start":
		if event.Message == nil {
			return current, dialect.Recognised{}, nil
		}
		next := current
		if event.Message.Usage != nil {
			next.InputTokens = event.Message.Usage.InputTokens
		}
		return next, dialect.Recognised{
			Pieces: []base.Piece{base.Start(event.Message.ID, event.Message.Model, 0)},
		}, nil

	case "content_block_start":
		if event.ContentBlock == nil {
			return current, dialect.Recognised{}, nil
		}
		block := *event.ContentBlock
		kind := kindOf(block.Type)
		if kind == "" {
			return current, dialect.Recognised{Drops: unmappedBlock(block.Type, event.Index)}, nil
		}
		pieces := []base.Piece{base.OpenPiece(dialect.Key(event.Index), kind)}
		pieces = append(pieces, openPieces(event.Index, block)...)
		return current, dialect.Recognised{Pieces: pieces, Drops: blockDrops(event.RawBlock, block, event.Index)}, nil

	case "content_block_delta":
		if event.Delta == nil {
			return current, dialect.Recognised{}, nil
		}
		return current, dialect.Recognised{Pieces: deltaPieces(event.Index, *event.Delta)}, nil

	case "content_block_stop":
		return current, dialect.Recognised{Pieces: []base.Piece{base.ClosePiece(dialect.Key(event.Index))}}, nil

	case "message_delta":
		pieces := []base.Piece{}
		if event.Usage != nil {
			pieces = append(pieces, base.UsagePiece(&base.Usage{
				InputTokens:  current.InputTokens,
				OutputTokens: event.Usage.OutputTokens,
				TotalTokens:  current.InputTokens + event.Usage.OutputTokens,
			}))
		}
		if event.Delta != nil {
			pieces = append(pieces, dialect.Outcome(event.Delta.StopReason, stopReasons))
		}
		return current, dialect.Recognised{Pieces: pieces}, nil
	}
	return current, dialect.Recognised{}, nil
}

func bodyPieces(body []byte) (dialect.Recognised, error) {
	var message wireMessageResponse
	if err := json.Unmarshal(body, &message); err != nil {
		return dialect.Recognised{}, fmt.Errorf("%s: %w", Name, err)
	}
	if message.Error != nil {
		return dialect.Recognised{Pieces: []base.Piece{
			base.Start(message.ID, message.Model, 0),
			dialect.Failure(message.Error),
		}}, nil
	}
	pieces := []base.Piece{base.Start(message.ID, message.Model, 0)}
	skipped := []base.Drop{}
	for index, block := range message.Content {
		kind := kindOf(block.Type)
		if kind == "" {
			skipped = append(skipped, unmappedBlock(block.Type, index)...)
			continue
		}
		pieces = append(pieces, base.OpenPiece(dialect.Key(index), kind))
		pieces = append(pieces, openPieces(index, block)...)
		pieces = append(pieces, contentPieces(index, block)...)
		pieces = append(pieces, base.ClosePiece(dialect.Key(index)))
	}
	if message.Usage != nil {
		pieces = append(pieces, base.UsagePiece(&base.Usage{
			InputTokens:  message.Usage.InputTokens,
			OutputTokens: message.Usage.OutputTokens,
			TotalTokens:  message.Usage.InputTokens + message.Usage.OutputTokens,
		}))
	}
	pieces = append(pieces, dialect.Outcome(message.StopReason, stopReasons))

	drops := skipped
	for index, raw := range message.RawContent {
		if index < len(message.Content) && kindOf(message.Content[index].Type) != "" {
			drops = append(drops, blockDrops(raw, message.Content[index], index)...)
		}
	}
	return dialect.Recognised{Pieces: pieces, Drops: drops}, nil
}

// openPieces carries the identity a block announces the moment it opens.
func openPieces(index int, block wireBlock) []base.Piece {
	pieces := []base.Piece{}
	if block.Type == "tool_use" {
		pieces = append(pieces, base.CallPiece(dialect.Key(index), block.ID, block.Name))
	}
	if block.Type == "thinking" && block.Signature != "" {
		pieces = append(pieces, base.StatePiece(dialect.Key(index), block.Signature))
	}
	return pieces
}

// contentPieces carries a whole block's content, for the buffered path where
// nothing arrived as a delta.
func contentPieces(index int, block wireBlock) []base.Piece {
	switch block.Type {
	case "text":
		if block.Text == "" {
			return nil
		}
		return []base.Piece{base.TextPiece(dialect.Key(index), block.Text)}
	case "thinking":
		if block.Thinking == "" {
			return nil
		}
		return []base.Piece{base.SummaryPiece(dialect.Key(index), block.Thinking)}
	case "tool_use":
		if len(block.Input) == 0 {
			return nil
		}
		return []base.Piece{base.ArgsPiece(dialect.Key(index), string(block.Input))}
	}
	return nil
}

func deltaPieces(index int, delta wireDelta) []base.Piece {
	switch delta.Type {
	case "text_delta":
		return []base.Piece{base.TextPiece(dialect.Key(index), delta.Text)}
	case "thinking_delta":
		return []base.Piece{base.SummaryPiece(dialect.Key(index), delta.Thinking)}
	case "signature_delta":
		return []base.Piece{base.StatePiece(dialect.Key(index), delta.Signature)}
	case "input_json_delta":
		return []base.Piece{base.ArgsPiece(dialect.Key(index), delta.PartialJSON)}
	}
	return nil
}

// outcomePiece maps a provider stop reason onto one of the three base
// statuses. An unknown value is not dragoman's business: it passes through as
// a completed response rather than crashing a decoder.
var stopReasons = map[string]string{
	"max_tokens":                    "max_output_tokens",
	"refusal":                       "refusal",
	"model_context_window_exceeded": "model_context_window_exceeded",
}

// kindOf maps a block type onto a base item. Anthropic emits block types
// dragoman does not move — redacted_thinking, server tool results — and
// guessing one of them into a message would put an empty assistant turn in the
// answer. An unmapped block is skipped and reported instead.
func kindOf(blockType string) base.ItemType {
	return blockKinds[blockType]
}

func unmappedBlock(blockType string, index int) []base.Drop {
	return []base.Drop{dialect.Unmapped(dialect.IndexedPath("content", index), "block type", blockType)}
}

var blockKinds = map[string]base.ItemType{
	"text":     base.ItemMessage,
	"thinking": base.ItemReasoning,
	"tool_use": base.ItemFunctionCall,
}

func blockDrops(raw json.RawMessage, block wireBlock, index int) []base.Drop {
	return dialect.ShapeDrops(raw, block, dialect.IndexedPath("content", index))
}

type wireRequest struct {
	Model        string            `json:"model,omitempty"`
	MaxTokens    int               `json:"max_tokens"`
	System       string            `json:"system,omitempty"`
	Messages     []wireMessage     `json:"messages"`
	Tools        []wireTool        `json:"tools,omitempty"`
	ToolChoice   *wireToolChoice   `json:"tool_choice,omitempty"`
	OutputConfig *wireOutputConfig `json:"output_config,omitempty"`
	Stream       bool              `json:"stream"`
}

type wireMessage struct {
	Role    string      `json:"role"`
	Content []wireBlock `json:"content"`
}

type wireBlock struct {
	Type string `json:"type"`

	Text string `json:"text,omitempty"`

	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`

	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`

	// Caller rides on every tool_use block. Modelling it keeps it out of the
	// drop diff on every single tool call; it is optional on the way back.
	Caller *wireCaller `json:"caller,omitempty"`
}

type wireCaller struct {
	Type string `json:"type"`
}

// MarshalJSON emits only the fields that belong to a block's type. A thinking
// block must carry its text even when the backend returned none, because the
// field is required on the way back in; a text block must not carry it at all.
// Selection is a dispatch table rather than a switch (R-4).
func (b wireBlock) MarshalJSON() ([]byte, error) {
	if shape, known := blockShapes[b.Type]; known {
		return json.Marshal(shape(b))
	}
	type plain wireBlock
	return json.Marshal(plain(b))
}

var blockShapes = map[string]func(wireBlock) any{
	"text": func(b wireBlock) any {
		return struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{b.Type, b.Text}
	},
	"thinking": func(b wireBlock) any {
		return struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}{b.Type, b.Thinking, b.Signature}
	},
	"tool_use": func(b wireBlock) any {
		// Caller is omitted when dragoman builds the block and present when it
		// decoded one, which keeps an outbound request clean while still
		// showing the diff that the field was consumed.
		return struct {
			Type   string          `json:"type"`
			ID     string          `json:"id"`
			Name   string          `json:"name"`
			Input  json.RawMessage `json:"input"`
			Caller *wireCaller     `json:"caller,omitempty"`
		}{b.Type, b.ID, b.Name, b.Input, b.Caller}
	},
	"tool_result": func(b wireBlock) any {
		return struct {
			Type      string `json:"type"`
			ToolUseID string `json:"tool_use_id"`
			Content   string `json:"content"`
		}{b.Type, b.ToolUseID, b.Content}
	},
}

type wireTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

type wireToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse *bool  `json:"disable_parallel_tool_use,omitempty"`
}

type wireOutputConfig struct {
	Effort string      `json:"effort,omitempty"`
	Format *wireFormat `json:"format,omitempty"`
}

type wireFormat struct {
	Type   string          `json:"type"`
	Schema json.RawMessage `json:"schema,omitempty"`
}

type wireMessageResponse struct {
	ID         string             `json:"id"`
	Model      string             `json:"model"`
	Content    []wireBlock        `json:"content"`
	RawContent []json.RawMessage  `json:"-"`
	StopReason string             `json:"stop_reason"`
	Usage      *wireUsage         `json:"usage"`
	Error      *dialect.WireError `json:"error"`
}

func (m *wireMessageResponse) UnmarshalJSON(data []byte) error {
	type plain wireMessageResponse
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*m = wireMessageResponse(decoded)
	m.RawContent = dialect.RawElements(data, "content")
	return nil
}

type wireUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

type wireEvent struct {
	Type         string             `json:"type"`
	Error        *dialect.WireError `json:"error"`
	Index        int                `json:"index"`
	Message      *wireStartMsg      `json:"message"`
	ContentBlock *wireBlock         `json:"content_block"`
	RawBlock     json.RawMessage    `json:"-"`
	Delta        *wireDelta         `json:"delta"`
	Usage        *wireUsage         `json:"usage"`
}

func (e *wireEvent) UnmarshalJSON(data []byte) error {
	type plain wireEvent
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*e = wireEvent(decoded)
	e.RawBlock = dialect.RawField(data, "content_block")
	return nil
}

type wireStartMsg struct {
	ID    string     `json:"id"`
	Model string     `json:"model"`
	Usage *wireUsage `json:"usage"`
}

type wireDelta struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Thinking    string `json:"thinking"`
	Signature   string `json:"signature"`
	PartialJSON string `json:"partial_json"`
	StopReason  string `json:"stop_reason"`
}
