package base

import (
	"encoding/json"
	"fmt"
	"strings"
)

type ItemType string

const (
	ItemMessage            ItemType = "message"
	ItemFunctionCall       ItemType = "function_call"
	ItemFunctionCallOutput ItemType = "function_call_output"
	ItemReasoning          ItemType = "reasoning"
)

const (
	RoleUser      = "user"
	RoleAssistant = "assistant"
	RoleSystem    = "system"
	RoleDeveloper = "developer"
)

const (
	PartInputText  = "input_text"
	PartOutputText = "output_text"
	PartSummary    = "summary_text"
)

type Item struct {
	Type   ItemType `json:"type"`
	ID     string   `json:"id,omitempty"`
	Status string   `json:"status,omitempty"`

	Role    string        `json:"role,omitempty"`
	Content []ContentPart `json:"content,omitempty"`

	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	Output string `json:"output,omitempty"`

	Summary []ContentPart `json:"summary,omitempty"`

	// Providers require their own state back unaltered and in position.
	EncryptedContent string `json:"encrypted_content,omitempty"`
}

type ContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`

	// Placing a breakpoint buys a cache write, so this holds only what the
	// caller sent.
	PromptCacheBreakpoint json.RawMessage `json:"prompt_cache_breakpoint,omitempty"`
}

// A client that serialises its unset fields writes the key with null in it, and
// one modelling the flag as a boolean writes false to mean do not cache here.
// Anthropic caps a request at four cached blocks, so a marker placed off either
// spelling is a cache write nobody asked for.
func (p ContentPart) Marks() bool {
	var marker map[string]any
	if json.Unmarshal(p.PromptCacheBreakpoint, &marker) != nil {
		return false
	}
	return len(marker) > 0
}

func GetText(item Item) string {
	return joinParts(item.Content)
}

func GetSummary(item Item) string {
	return joinParts(item.Summary)
}

func FilterItems(items []Item, keep func(Item) bool) []Item {
	kept := make([]Item, 0, len(items))
	for _, item := range items {
		if keep(item) {
			kept = append(kept, item)
		}
	}
	return kept
}

func (i Item) MarshalJSON() ([]byte, error) {
	shape, known := itemShapes[i.Type]
	if !known {
		// A provider is free to add an item type dragoman does not move.
		return json.Marshal(unknownShape{Type: i.Type, ID: i.ID, Status: i.Status})
	}
	return json.Marshal(shape(i))
}

func IsKnownItemType(itemType ItemType) bool {
	_, known := itemShapes[itemType]
	return known
}

func (i *Item) UnmarshalJSON(data []byte) error {
	var wire wireItem
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*i = Item{
		Type:             wire.Type,
		ID:               wire.ID,
		Status:           wire.Status,
		Role:             wire.Role,
		CallID:           wire.CallID,
		Name:             wire.Name,
		Arguments:        wire.Arguments,
		Summary:          wire.Summary,
		EncryptedContent: wire.EncryptedContent,
	}
	if i.Type == "" {
		i.Type = ItemMessage
	}
	content, err := decodeContent(wire.Content)
	if err != nil {
		return err
	}
	i.Content = content
	output, err := decodeOutput(wire.Output)
	if err != nil {
		return err
	}
	i.Output = output
	return nil
}

type wireItem struct {
	Type             ItemType        `json:"type"`
	ID               string          `json:"id"`
	Status           string          `json:"status"`
	Role             string          `json:"role"`
	Content          json.RawMessage `json:"content"`
	CallID           string          `json:"call_id"`
	Name             string          `json:"name"`
	Arguments        string          `json:"arguments"`
	Output           json.RawMessage `json:"output"`
	Summary          []ContentPart   `json:"summary"`
	EncryptedContent string          `json:"encrypted_content"`
}

var itemShapes = map[ItemType]func(Item) any{
	ItemMessage: func(i Item) any {
		return messageShape{
			Type: i.Type, ID: i.ID, Status: i.Status,
			Role: i.Role, Content: nonNilParts(i.Content),
		}
	},
	ItemFunctionCall: func(i Item) any {
		return callShape{
			Type: i.Type, ID: i.ID, Status: i.Status,
			CallID: i.CallID, Name: i.Name, Arguments: i.Arguments,
			EncryptedContent: i.EncryptedContent,
		}
	},
	ItemFunctionCallOutput: func(i Item) any {
		return callOutputShape{Type: i.Type, ID: i.ID, CallID: i.CallID, Output: i.Output}
	},
	ItemReasoning: func(i Item) any {
		return reasoningShape{
			Type: i.Type, ID: i.ID, Status: i.Status,
			Summary:          nonNilParts(i.Summary),
			Content:          []ContentPart{},
			EncryptedContent: i.EncryptedContent,
		}
	},
}

type unknownShape struct {
	Type   ItemType `json:"type"`
	ID     string   `json:"id,omitempty"`
	Status string   `json:"status,omitempty"`
}

type messageShape struct {
	Type    ItemType      `json:"type"`
	ID      string        `json:"id,omitempty"`
	Status  string        `json:"status,omitempty"`
	Role    string        `json:"role"`
	Content []ContentPart `json:"content"`
}

type callShape struct {
	Type      ItemType `json:"type"`
	ID        string   `json:"id,omitempty"`
	Status    string   `json:"status,omitempty"`
	CallID    string   `json:"call_id"`
	Name      string   `json:"name"`
	Arguments string   `json:"arguments"`
	// Gemini binds its thought signature to the call rather than to a preceding
	// reasoning item.
	EncryptedContent string `json:"encrypted_content,omitempty"`
}

type callOutputShape struct {
	Type   ItemType `json:"type"`
	ID     string   `json:"id,omitempty"`
	CallID string   `json:"call_id"`
	Output string   `json:"output"`
}

type reasoningShape struct {
	Type             ItemType      `json:"type"`
	ID               string        `json:"id,omitempty"`
	Status           string        `json:"status,omitempty"`
	Summary          []ContentPart `json:"summary"`
	Content          []ContentPart `json:"content"`
	EncryptedContent string        `json:"encrypted_content,omitempty"`
}

func joinParts(parts []ContentPart) string {
	var joined strings.Builder
	for _, part := range parts {
		joined.WriteString(part.Text)
	}
	return joined.String()
}

func nonNilParts(parts []ContentPart) []ContentPart {
	if parts == nil {
		return []ContentPart{}
	}
	return parts
}

// The wire carries content as a bare string or as a parts array.
func decodeContent(raw json.RawMessage) ([]ContentPart, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []ContentPart{{Type: PartInputText, Text: text}}, nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("content: %w", err)
	}
	return parts, nil
}

// A function_call_output arrives with its output as a bare string or a parts
// array.
func decodeOutput(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var parts []ContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", fmt.Errorf("output: %w", err)
	}
	return joinParts(parts), nil
}
