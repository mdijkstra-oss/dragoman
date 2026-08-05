// Package anthropicmessages speaks POST /v1/messages. It is the only backend
// that requires an output cap, and the only one that nests a tool result inside
// a user message rather than making it a sibling item.
package anthropicmessages

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mdijkstra-oss/dragoman/internal/base"
	"github.com/mdijkstra-oss/dragoman/internal/dialect"
)

const Name = "anthropic-messages"

// Version is the API version header the endpoint requires; without it the
// request is rejected before it is read.
const Version = "2023-06-01"

// On a reasoning model the cap covers thinking and output together, so a small
// one truncates mid-thought.
const DefaultMaxTokens = 20000

func Dialect() dialect.Dialect {
	return dialect.Dialect{
		Name:   Name,
		Path:   "/messages",
		KeyEnv: "ANTHROPIC_API_KEY",
		// This endpoint's own SDKs require streaming at a cap this size, to
		// avoid HTTP timeouts.
		StreamUpstream: true,
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
			// A cache_control has a type and a TTL, nothing meaning OpenAI's
			// mode, and no cache key of any kind.
			"mode":             base.DropUnsupported,
			"prompt_cache_key": base.DropUnsupported,
		}),
	}
}

var carries = dialect.Carries{
	"model", "instructions", "input", "tools", "tool_choice",
	"text.format", "reasoning.effort", "max_output_tokens", "stream",
	"parallel_tool_calls", "prompt_cache_options",
}

func encode(req base.Request, backend base.Backend, stream bool) (dialect.Encoded, error) {
	drops, err := base.Unmodelled(req.Raw, req)
	if err != nil {
		return dialect.Encoded{}, fmt.Errorf("%s: %w", Name, err)
	}
	drops = append(drops, dialect.Unread(req, carries)...)

	if err := checkTurns(req, backend); err != nil {
		return dialect.Encoded{}, err
	}

	cap, capDrops := outputCap(req, backend)
	drops = append(drops, capDrops...)

	body := wireRequest{Model: base.GetModel(req), MaxTokens: cap, Stream: stream}

	system, messages, itemDrops, err := buildMessages(req)
	if err != nil {
		return dialect.Encoded{}, fmt.Errorf("%s: %w", Name, err)
	}
	body.System, body.Messages = system, messages
	drops = append(drops, itemDrops...)
	drops = append(drops, markerDrops(req.Input)...)
	drops = append(drops, optionDrops(req.PromptCacheOptions, placedAny(body.Messages))...)

	drops = append(drops, applyTools(&body, req, backend)...)

	if effort := base.GetEffort(req); effort != "" {
		body.OutputConfig = &wireOutputConfig{Effort: effort}
	}
	if format := base.GetFormat(req); format != nil && format.Type == "json_schema" {
		if body.OutputConfig == nil {
			body.OutputConfig = &wireOutputConfig{}
		}
		body.OutputConfig.Format = &wireFormat{Type: format.Type, Schema: format.Schema}
		if format.Name != "" {
			drops = append(drops, base.Drop{Path: "text.format.name", Reason: base.DropInert,
				Detail: Name + " has no name on a structured-output format, and a label on a schema changes no answer"})
		}
		if format.Strict != nil {
			drops = append(drops, strictness(*format.Strict))
		}
	}

	encoded, err := json.Marshal(body)
	if err != nil {
		return dialect.Encoded{}, fmt.Errorf("%s: encode: %w", Name, err)
	}
	return dialect.Encoded{Body: encoded, Drops: drops}, nil
}

// strictness reports what became of the strict flag. This dialect carries
// strictness per tool rather than per format, and a structured-output format
// constrains decoding whether or not it was asked to, so asking for strict is
// asking for what the request already gets; only asking to relax it is lost.
func strictness(strict bool) base.Drop {
	if strict {
		return base.Drop{Path: "text.format.strict", Reason: base.DropInert,
			Detail: Name + " carries strictness per tool, and a format is always enforced"}
	}
	return base.Drop{Path: "text.format.strict", Reason: base.DropUnsupported,
		Detail: Name + " carries strictness per tool: the format is enforced though the request asked it not to be"}
}

// This wire format will not accept a request without an output cap. The
// exception is the endpoint with nowhere to put one.
func outputCap(req base.Request, backend base.Backend) (*int, []base.Drop) {
	if backend.NoOutputCap {
		if req.MaxOutputTokens == nil {
			return nil, nil
		}
		return nil, []base.Drop{cannot(backend, "max_output_tokens", "has no output cap")}
	}
	cap := DefaultMaxTokens
	if req.MaxOutputTokens != nil {
		cap = *req.MaxOutputTokens
	}
	return &cap, nil
}

// A backend that resolves tool calls itself has no channel to hand one back, so
// a tool sent there produces prose where the caller expected a call.
func applyTools(body *wireRequest, req base.Request, backend base.Backend) []base.Drop {
	if backend.NoTools {
		return toolless(req, backend)
	}
	tools, drops := dialect.FunctionTools(req.Tools)
	for _, tool := range tools {
		body.Tools = append(body.Tools, wireTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: tool.Parameters,
			Strict:      tool.Strict,
		})
	}
	body.ToolChoice = toolChoice(req)
	return drops
}

func toolless(req base.Request, backend base.Backend) []base.Drop {
	drops := []base.Drop{}
	for index := range req.Tools {
		drops = append(drops, cannot(backend,
			dialect.IndexedPath("tools", index), "resolves its own tools and cannot return a call to the caller"))
	}
	if req.ToolChoice != nil {
		drops = append(drops, cannot(backend, "tool_choice", "has no tools to choose between"))
	}
	if req.ParallelToolCalls != nil {
		drops = append(drops, cannot(backend, "parallel_tool_calls", "has no tools to run in parallel"))
	}
	return drops
}

// A one-turn backend runs a turn per user message. Dropping one changes the
// conversation and sending one runs an extra turn nobody asked for.
func checkTurns(req base.Request, backend base.Backend) error {
	if !backend.OneTurnPerRequest {
		return nil
	}
	starts := userTurnStarts(req.Input)
	if len(starts) <= 1 {
		return nil
	}
	paths := make([]string, 0, len(starts))
	for _, index := range starts {
		paths = append(paths, dialect.IndexedPath("input", index))
	}
	return fmt.Errorf("%s executes one turn per request; input has %d user turns, at %s",
		backend.Name, len(starts), strings.Join(paths, ", "))
}

func userTurnStarts(items []base.Item) []int {
	starts := []int{}
	previous := ""
	for index, item := range items {
		role := turnRole(item)
		if role == "" {
			continue
		}
		if role == base.RoleUser && previous != base.RoleUser {
			starts = append(starts, index)
		}
		previous = role
	}
	return starts
}

// turnRole reports which side of the conversation an input item joins, or the
// empty string when it feeds the system prompt or carries nothing to send.
// Turn membership is decided in one place so that what the encoder groups and
// what the turn check counts can never disagree.
func turnRole(item base.Item) string {
	decide, moved := turnRoles[item.Type]
	if !moved {
		return ""
	}
	return decide(item)
}

var turnRoles = map[base.ItemType]func(base.Item) string{
	base.ItemMessage:            messageTurn,
	base.ItemReasoning:          reasoningTurn,
	base.ItemFunctionCall:       assistantTurn,
	base.ItemFunctionCallOutput: userTurn,
}

func messageTurn(item base.Item) string {
	if item.Role == base.RoleSystem || item.Role == base.RoleDeveloper {
		return ""
	}
	if item.Role == base.RoleAssistant {
		return base.RoleAssistant
	}
	return base.RoleUser
}

// reasoningTurn joins the assistant side only when the item still carries the
// state the backend wants back; a summary alone is not sent at all.
func reasoningTurn(item base.Item) string {
	if item.EncryptedContent == "" {
		return ""
	}
	return base.RoleAssistant
}

func assistantTurn(base.Item) string { return base.RoleAssistant }

func userTurn(base.Item) string { return base.RoleUser }

func cannot(backend base.Backend, path, detail string) base.Drop {
	return base.Drop{Path: path, Reason: base.DropUnsupported, Detail: backend.Name + " " + detail}
}

// A tool result must sit in the user message immediately following the assistant
// message holding its call.
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
			role := turnRole(item)
			if role == "" {
				system = dialect.JoinText(system, base.GetText(item))
				return
			}
			for _, block := range textBlocks(item, base.GetCacheTTL(req)) {
				appendBlock(role, block)
			}
		},
		Reasoning: func(item base.Item) {
			// The signature goes back byte-identical or the request is rejected.
			if turnRole(item) == "" {
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

// textBlocks renders a message item's content. It is the one joined block it has
// always been unless a part carries a cache breakpoint: cache_control sits on a
// block, so marking the third of five parts means five blocks with the marker on
// the third. Nothing is trimmed at four — the limit is this endpoint's to
// enforce, and a fifth is forwarded so the refusal names it.
func textBlocks(item base.Item, ttl *string) []wireBlock {
	if !dialect.Marked(item.Content) {
		return []wireBlock{{Type: "text", Text: base.GetText(item)}}
	}
	blocks := make([]wireBlock, 0, len(item.Content))
	for _, part := range item.Content {
		if part.Text == "" {
			// A part carrying no text is left out, exactly as the joined form
			// left it out. Spelling one out would mint a block nobody sent —
			// this endpoint refuses an empty text block, so a marker elsewhere
			// in the message would turn a request that worked into a 400, and a
			// marker on this part would land the caller's prefix boundary on a
			// block dragoman invented. What could not be placed is reported.
			continue
		}
		block := wireBlock{Type: "text", Text: part.Text}
		if part.Marks() {
			// The TTL is request-wide at the other end and per breakpoint here,
			// so one value fans out onto every marker. A type alone gets this
			// endpoint's default, which is why a spelled-out 30m is refused
			// where omitting it succeeds.
			block.CacheControl = &wireCacheControl{Type: "ephemeral", TTL: ttl}
		}
		blocks = append(blocks, block)
	}
	if len(blocks) == 0 {
		return []wireBlock{{Type: "text", Text: base.GetText(item)}}
	}
	return blocks
}

// markerDrops reports what a cache_control could not carry from each breakpoint
// the caller placed. The marker's presence is the whole of what travels, so the
// bytes inside it are diffed away rather than listed by hand, and a mode arrives
// as a path the caller can find.
func markerDrops(items []base.Item) []base.Drop {
	drops := []base.Drop{}
	for index, item := range items {
		for position, part := range item.Content {
			if !part.Marks() {
				continue
			}
			path := dialect.MarkerPath(index, position)
			if part.Text == "" {
				// A cache_control rides on a text block, and this part has no
				// text to make one out of.
				drops = append(drops, base.Drop{Path: path, Reason: base.DropUnsupported,
					Detail: Name + " marks a text block, and this part carries no text"})
				continue
			}
			if turnRole(item) == "" {
				// The system prompt is one string here, so once the text is
				// folded in there is no block left to mark.
				drops = append(drops, base.Drop{Path: path, Reason: base.DropUnimplemented,
					Detail: Name + " keeps instructions in system, which carries no block to mark"})
				continue
			}
			drops = append(drops, dialect.ShapeDrops(part.PromptCacheBreakpoint, struct{}{}, path)...)
		}
	}
	return drops
}

func placedAny(messages []wireMessage) bool {
	for _, message := range messages {
		for _, block := range message.Content {
			if block.CacheControl != nil {
				return true
			}
		}
	}
	return false
}

// optionDrops reports what a cache_control could not carry from the request-wide
// options. Only the TTL travels, and only onto a marker that was placed: a TTL
// on a request that marked nothing is a value the caller sent and the wire never
// saw, so claiming it as consumed would leave --strict calling that run clean.
// Whatever else arrived is named by the diff rather than by a list kept here.
func optionDrops(options *base.PromptCacheOptions, placed bool) []base.Drop {
	if options == nil {
		return nil
	}
	carried := struct {
		TTL *string `json:"ttl,omitempty"`
	}{}
	if placed {
		carried.TTL = options.TTL
	}
	return dialect.ShapeDrops(options.Raw, carried, "prompt_cache_options")
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

// The two halves of usage arrive on different frames: input counts at
// message_start, the rest at the terminal frame.
type decodeState struct {
	InputTokens   int
	CachedTokens  int
	WrittenTokens int
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
		if usage := event.Message.Usage; usage != nil {
			next.InputTokens = usage.InputTokens
			next.CachedTokens = usage.CacheReadInputTokens
			next.WrittenTokens = usage.CacheCreationInputTokens
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
		if usage := event.Usage; usage != nil {
			pieces = append(pieces, base.UsagePiece(base.CountUsage(
				current.InputTokens, usage.OutputTokens,
				usage.Details.ThinkingTokens, current.CachedTokens, current.WrittenTokens)))
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
	if usage := message.Usage; usage != nil {
		pieces = append(pieces, base.UsagePiece(base.CountUsage(
			usage.InputTokens, usage.OutputTokens,
			usage.Details.ThinkingTokens, usage.CacheReadInputTokens, usage.CacheCreationInputTokens)))
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

var stopReasons = map[string]string{
	"max_tokens":                    "max_output_tokens",
	"refusal":                       "refusal",
	"model_context_window_exceeded": "model_context_window_exceeded",
}

// Anthropic emits block types dragoman does not move — redacted_thinking, server
// tool results — and guessing one into a message would put an empty assistant
// turn in the answer.
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
	Model string `json:"model,omitempty"`

	// One endpoint serving this shape requires a cap and another has nowhere to
	// put it. Zero is not a cap either would accept.
	MaxTokens *int `json:"max_tokens,omitempty"`

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

	// Rides on every tool_use block, and is optional on the way back.
	Caller *wireCaller `json:"caller,omitempty"`

	// CacheControl is this dialect's breakpoint, placed on the block the caller
	// marked. Never on one it did not: a breakpoint is a spending decision.
	CacheControl *wireCacheControl `json:"cache_control,omitempty"`
}

type wireCaller struct {
	Type string `json:"type"`
}

// wireCacheControl marks a block as the end of a cacheable prefix. The TTL is
// placed verbatim, and this endpoint takes only 5m and 1h: a 30m is forwarded so
// the refusal names the field, because converting it would be dragoman choosing
// a price. Absent stays absent, leaving the endpoint's own default.
type wireCacheControl struct {
	Type string  `json:"type"`
	TTL  *string `json:"ttl,omitempty"`
}

// A thinking block must carry its text even when the backend returned none,
// because the field is required on the way back in. A text block must not carry
// it at all.
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
			Type         string            `json:"type"`
			Text         string            `json:"text"`
			CacheControl *wireCacheControl `json:"cache_control,omitempty"`
		}{b.Type, b.Text, b.CacheControl}
	},
	"thinking": func(b wireBlock) any {
		return struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}{b.Type, b.Thinking, b.Signature}
	},
	"tool_use": func(b wireBlock) any {
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
	InputTokens          int `json:"input_tokens"`
	OutputTokens         int `json:"output_tokens"`
	CacheReadInputTokens int `json:"cache_read_input_tokens"`
	// This endpoint also splits the writes by TTL bucket under cache_creation.
	// The base reads the flat total and not the buckets.
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	Details                  struct {
		ThinkingTokens int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
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
