// Package openairesponses speaks the dialect the base is written in. Encoding
// is therefore the arriving body with the overrides applied to it, and decoding
// recognises the grammar the emitter itself produces.
package openairesponses

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/matthijn/dragoman/internal/base"
	"github.com/matthijn/dragoman/internal/dialect"
)

// Name is the dialect's value on the command line.
const Name = "openai-responses"

func Dialect() dialect.Dialect {
	return dialect.Dialect{
		Name:   Name,
		Path:   "/responses",
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
		DropTable:    map[string]base.DropReason{},
	}
}

func encode(req base.Request, backend base.Backend, stream bool) (dialect.Encoded, error) {
	changes := map[string]any{}

	// Compare against what the body means rather than against which keys it
	// happens to carry: an absent stream already means false, so writing one
	// in would rewrite a body that needed no change.
	if stream != base.GetStream(req) {
		changes["stream"] = stream
	}
	if model := base.GetModel(req); model != "" && base.Differs(req.Raw, "model", model) {
		changes["model"] = model
	}
	if req.Reasoning != nil && base.Differs(req.Raw, "reasoning", req.Reasoning) {
		changes["reasoning"] = req.Reasoning
	}
	if cap := req.MaxOutputTokens; cap != nil && base.Differs(req.Raw, "max_output_tokens", *cap) {
		changes["max_output_tokens"] = *cap
	}
	body, err := base.Patch(req.Raw, changes)
	if err != nil {
		return dialect.Encoded{}, fmt.Errorf("%s: encode: %w", Name, err)
	}
	if len(req.Raw) == 0 {
		body, err = json.Marshal(req)
		if err != nil {
			return dialect.Encoded{}, fmt.Errorf("%s: encode: %w", Name, err)
		}
	}
	// The body the backend receives is the body the caller sent with the
	// overrides written onto it, so nothing the base did not model is lost on
	// this edge: an override moves the top-level key order and touches no
	// value it did not name.
	return dialect.Encoded{Body: body}, nil
}

// decodeState remembers which items already streamed their content, so that a
// terminal item frame completes a truncated item without doubling one that
// arrived as deltas.
type decodeState struct {
	Streamed []string
}

func streamPieces(state dialect.State, frame base.Frame) (dialect.State, dialect.Recognised, error) {
	current, ok := state.(decodeState)
	if !ok {
		return state, dialect.Recognised{}, fmt.Errorf("%s: bad decoder state", Name)
	}
	if len(frame.Data) == 0 || string(frame.Data) == "[DONE]" {
		return current, dialect.Recognised{}, nil
	}
	var event wireEvent
	if err := json.Unmarshal(frame.Data, &event); err != nil {
		return current, dialect.Recognised{}, fmt.Errorf("%s: event: %w", Name, err)
	}

	switch base.EventType(event.Type) {
	case base.EventCreated:
		if event.Response == nil {
			return current, dialect.Recognised{}, nil
		}
		return current, pieces(base.Start(event.Response.ID, event.Response.Model, event.Response.CreatedAt)), nil

	case base.EventItemAdded:
		item, present := event.item()
		if !present {
			return current, dialect.Recognised{}, nil
		}
		if !base.IsKnownItemType(item.Type) {
			return current, dialect.Recognised{Drops: unmappedItem(item, itemPath(event.OutputIndex))}, nil
		}
		return current, pieces(base.OpenIdentified(item.ID, item.Type, item.ID)), nil

	case base.EventTextDelta:
		return mark(current, event.ItemID), pieces(base.TextPiece(event.ItemID, event.Delta)), nil

	case base.EventArgumentsDelta:
		return mark(current, event.ItemID), pieces(base.ArgsPiece(event.ItemID, event.Delta)), nil

	case base.EventSummaryTextDelta:
		return mark(current, event.ItemID), pieces(base.SummaryPiece(event.ItemID, event.Delta)), nil

	case base.EventItemDone:
		item, present := event.item()
		if !present || !base.IsKnownItemType(item.Type) {
			return current, dialect.Recognised{}, nil
		}
		return current, dialect.Recognised{
			Pieces: closePieces(item, streamedContent(current, item.ID)),
			Drops:  dialect.ShapeDrops(event.Item, item, itemPath(event.OutputIndex)),
		}, nil

	case base.EventCompleted, base.EventIncomplete, base.EventFailed:
		if event.Response == nil {
			return current, dialect.Recognised{}, nil
		}
		return current, dialect.Recognised{Pieces: outcomePieces(*event.Response)}, nil
	}
	return current, dialect.Recognised{}, nil
}

func bodyPieces(body []byte) (dialect.Recognised, error) {
	resp, err := base.ParseResponse(body)
	if err != nil {
		return dialect.Recognised{}, fmt.Errorf("%s: %w", Name, err)
	}
	built := []base.Piece{base.Start(resp.ID, resp.Model, resp.CreatedAt)}
	skipped := []base.Drop{}
	for index, item := range resp.Output {
		if !base.IsKnownItemType(item.Type) {
			skipped = append(skipped, unmappedItem(item, dialect.IndexedPath("output", index))...)
			continue
		}
		built = append(built, base.OpenIdentified(item.ID, item.Type, item.ID))
		built = append(built, closePieces(item, false)...)
	}
	built = append(built, outcomePieces(resp)...)

	// The diff is scoped to the answer-bearing subtree. A request echo the
	// backend replays is not part of the answer, and reporting it would bury
	// the drop that does produce a confident, well-formed, wrong result.
	var envelope struct {
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return dialect.Recognised{}, fmt.Errorf("%s: %w", Name, err)
	}
	drops, err := base.Diff(envelope.Output, resp.Output)
	if err != nil {
		return dialect.Recognised{}, fmt.Errorf("%s: %w", Name, err)
	}
	return dialect.Recognised{Pieces: built, Drops: append(skipped, dialect.Prefixed(drops, "output")...)}, nil
}

// unmappedItem reports an item type dragoman does not move. The base carries
// four; anything else belongs to a capability that is out of scope, and the
// caller is told rather than left with a silently shorter answer.
func unmappedItem(item base.Item, path string) []base.Drop {
	return []base.Drop{dialect.Unmapped(path, "item type", string(item.Type))}
}

func pieces(list ...base.Piece) dialect.Recognised {
	return dialect.Recognised{Pieces: list}
}

func itemPath(outputIndex *int) string {
	if outputIndex == nil {
		return "output"
	}
	return dialect.IndexedPath("output", *outputIndex)
}

// closePieces renders a complete item. Content already delivered as deltas is
// not repeated; a truncated item still gets everything the terminal frame
// carried.
func closePieces(item base.Item, alreadyStreamed bool) []base.Piece {
	pieces := []base.Piece{}
	if !alreadyStreamed {
		if text := base.GetText(item); text != "" {
			pieces = append(pieces, base.TextPiece(item.ID, text))
		}
		if summary := base.GetSummary(item); summary != "" {
			pieces = append(pieces, base.SummaryPiece(item.ID, summary))
		}
		if item.Type == base.ItemFunctionCall && item.Arguments != "" {
			pieces = append(pieces, base.ArgsPiece(item.ID, item.Arguments))
		}
	}
	if item.Type == base.ItemFunctionCall {
		pieces = append(pieces, base.CallPiece(item.ID, item.CallID, item.Name))
	}
	if item.EncryptedContent != "" {
		pieces = append(pieces, base.StatePiece(item.ID, item.EncryptedContent))
	}
	return append(pieces, base.ClosePiece(item.ID))
}

func outcomePieces(resp base.Response) []base.Piece {
	pieces := []base.Piece{}
	if resp.Usage != nil {
		pieces = append(pieces, base.UsagePiece(resp.Usage))
	}
	status := resp.Status
	if status == "" || status == base.StatusInProgress {
		status = base.StatusCompleted
	}
	if resp.Error != nil {
		status = base.StatusFailed
	}
	return append(pieces, base.OutcomePiece(status, resp.IncompleteDetails, resp.Error))
}

func mark(state decodeState, itemID string) decodeState {
	if streamedContent(state, itemID) {
		return state
	}
	return decodeState{Streamed: append(append([]string(nil), state.Streamed...), itemID)}
}

func streamedContent(state decodeState, itemID string) bool {
	return slices.Contains(state.Streamed, itemID)
}

type wireEvent struct {
	Type        string          `json:"type"`
	ItemID      string          `json:"item_id"`
	Delta       string          `json:"delta"`
	OutputIndex *int            `json:"output_index"`
	Item        json.RawMessage `json:"item"`
	Response    *base.Response  `json:"response"`
}

// item decodes the event's item, which every item-scoped frame carries whole.
func (e wireEvent) item() (base.Item, bool) {
	if len(e.Item) == 0 || string(e.Item) == "null" {
		return base.Item{}, false
	}
	var decoded base.Item
	if err := json.Unmarshal(e.Item, &decoded); err != nil {
		return base.Item{}, false
	}
	return decoded, true
}
