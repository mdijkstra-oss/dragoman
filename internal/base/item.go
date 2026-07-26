// Package base is the openai-responses format. It is not a format modelled on
// Responses; it is the same format. Dialect packages encode requests out of it
// and decode responses into it, and nothing else imports anything above it.
package base

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ItemType names the bounded set of items dragoman moves. No item type is ever
// dropped: every dialect has all four shapes in some spelling.
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

// Item is one element of a Responses input or output list. The fields are a
// union over the four item types; MarshalJSON emits only the ones that belong
// to Type, so a function_call never carries a role and a reasoning item always
// carries its summary array.
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

	// EncryptedContent is the provider state pocket. Whatever a backend
	// returned as state goes back to that backend unaltered and in position;
	// dragoman never inspects it.
	EncryptedContent string `json:"encrypted_content,omitempty"`
}

// ContentPart is one part of a message's content or a reasoning summary.
type ContentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// GetText concatenates every text part of a message item.
func GetText(item Item) string {
	return joinParts(item.Content)
}

// GetSummary concatenates every part of a reasoning item's summary.
func GetSummary(item Item) string {
	return joinParts(item.Summary)
}

// FilterItems keeps the items a predicate accepts, in order.
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
		// A provider is free to add an item type dragoman does not move. That
		// is the provider's set, not dragoman's, so it is reported as a drop
		// rather than crashing the decoder.
		return json.Marshal(unknownShape{Type: i.Type, ID: i.ID, Status: i.Status})
	}
	return json.Marshal(shape(i))
}

// IsKnownItemType reports whether dragoman moves this item type. Decoders use
// it to skip what they cannot map instead of guessing at it.
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

// itemShapes is a dispatch table rather than a switch so that adding an item
// type is a table entry (R-4). An absent entry panics (R-12).
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
	// A provider that binds its state to the call rather than to a preceding
	// reasoning item keeps it here. Provider state is a field on an item, and
	// the item it arrived on is the item it goes back on.
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

// joinParts concatenates the text of a part list.
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

// decodeContent accepts both the shorthand string form and the parts array.
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

// decodeOutput accepts a function_call_output whose output is a bare string or
// a parts array, and renders both as the string the base carries.
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
