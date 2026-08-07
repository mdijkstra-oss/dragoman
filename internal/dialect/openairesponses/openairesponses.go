// Package openairesponses speaks the dialect the base is written in, so encoding
// is the arriving body with the overrides applied to it.
package openairesponses

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/mdijkstra-oss/dragoman/internal/base"
	"github.com/mdijkstra-oss/dragoman/internal/dialect"
)

const Name = "openai-responses"

func Dialect() dialect.Dialect {
	return dialect.Dialect{
		Name: Name,
		Path: "/responses",
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

	// An absent stream already means false, so writing one in would rewrite a
	// body that needed no change.
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
	// An override moves the top-level key order and touches no value it did not
	// name.
	return dialect.Encoded{Body: body}, nil
}

type decodeState struct {
	Streamed []string
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

	switch base.EventType(event.Type) {
	case base.EventCreated:
		if event.Response == nil {
			return current, dialect.DecodedFrame{}, nil
		}
		return current, pieces(base.Start(event.Response.ID, event.Response.Model, event.Response.CreatedAt)), nil

	case base.EventItemAdded:
		item, present := event.item()
		if !present {
			return current, dialect.DecodedFrame{}, nil
		}
		if !base.IsKnownItemType(item.Type) {
			return current, dialect.DecodedFrame{Drops: unmappedItem(item, itemPath(event.OutputIndex))}, nil
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
			return current, dialect.DecodedFrame{}, nil
		}
		return current, dialect.DecodedFrame{
			Pieces: closePieces(item, streamedContent(current, item.ID)),
			Drops:  dialect.ShapeDrops(event.Item, item, itemPath(event.OutputIndex)),
		}, nil

	case base.EventCompleted, base.EventIncomplete, base.EventFailed:
		if event.Response == nil {
			return current, dialect.DecodedFrame{}, nil
		}
		return current, dialect.DecodedFrame{Pieces: outcomePieces(*event.Response)}, nil
	}
	return current, dialect.DecodedFrame{}, nil
}

func bodyPieces(body []byte) (dialect.DecodedFrame, error) {
	resp, err := base.ParseResponse(body)
	if err != nil {
		return dialect.DecodedFrame{}, fmt.Errorf("%s: %w", Name, err)
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

	// A request echo the backend replays is not part of the answer.
	var envelope struct {
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return dialect.DecodedFrame{}, fmt.Errorf("%s: %w", Name, err)
	}
	drops, err := base.Diff(envelope.Output, resp.Output)
	if err != nil {
		return dialect.DecodedFrame{}, fmt.Errorf("%s: %w", Name, err)
	}
	return dialect.DecodedFrame{Pieces: built, Drops: append(skipped, dialect.Prefixed(drops, "output")...)}, nil
}

func unmappedItem(item base.Item, path string) []base.Drop {
	return []base.Drop{dialect.Unmapped(path, "item type", string(item.Type))}
}

func pieces(list ...base.Piece) dialect.DecodedFrame {
	return dialect.DecodedFrame{Pieces: list}
}

func itemPath(outputIndex *int) string {
	if outputIndex == nil {
		return "output"
	}
	return dialect.IndexedPath("output", *outputIndex)
}

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

// Every item-scoped frame carries its item whole.
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
