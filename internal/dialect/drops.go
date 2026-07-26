package dialect

import (
	"encoding/json"
	"fmt"

	"github.com/matthijn/dragoman/internal/base"
)

// outOfScope names the base fields the design puts out of scope for every
// dialect. They are unsupported rather than unimplemented: current reasoning
// models reject or ignore the sampling parameters, token probabilities pin a
// caller to one model by construction, and logit bias is meaningless across
// providers because token ids are tokenizer-specific.
var outOfScope = []string{
	"temperature", "top_p", "top_k", "seed",
	"frequency_penalty", "presence_penalty", "logit_bias",
	"logprobs", "top_logprobs", "n",
}

// DropTable builds a dialect's classification table from the shared
// out-of-scope set plus whatever that dialect alone can never express.
func DropTable(own map[string]base.DropReason) map[string]base.DropReason {
	table := make(map[string]base.DropReason, len(outOfScope)+len(own))
	for _, field := range outOfScope {
		table[field] = base.DropUnsupported
	}
	for field, reason := range own {
		table[field] = reason
	}
	return table
}

// Prefixed re-roots drop paths under a parent, so a decoder can report where
// in the body a field was without every dialect spelling the join itself.
func Prefixed(drops []base.Drop, prefix string) []base.Drop {
	rooted := make([]base.Drop, 0, len(drops))
	for _, drop := range drops {
		switch {
		case drop.Path == "":
			drop.Path = prefix
		case drop.Path[0] == '[':
			drop.Path = prefix + drop.Path
		default:
			drop.Path = prefix + "." + drop.Path
		}
		rooted = append(rooted, drop)
	}
	return rooted
}

// Unmapped reports a wire shape the base does not carry — an item type, a
// content block, a hosted tool. The base moves four item types; anything else
// belongs to a capability that is out of scope, and the caller is told rather
// than left with a silently shorter answer.
func Unmapped(path, kind, name string) base.Drop {
	return base.Drop{
		Path:   path,
		Reason: base.DropUnimplemented,
		Detail: kind + " " + name + " is not carried by the base",
	}
}

// ShapeDrops diffs one wire value against the bytes it was decoded from and
// roots the result at path. Every decoder computes its drops this way rather
// than maintaining a list.
func ShapeDrops(raw []byte, typed any, path string) []base.Drop {
	if len(raw) == 0 {
		return nil
	}
	drops, err := base.Diff(raw, typed)
	if err != nil {
		return nil
	}
	return Prefixed(drops, path)
}

func IndexedPath(field string, index int) string {
	return fmt.Sprintf("%s[%d]", field, index)
}

func Key(index int) string { return fmt.Sprintf("%d", index) }

// ChoiceModes maps the base's tool-choice modes onto the spelling three of the
// four dialects share. The named form differs in all four and each places it
// itself.
var ChoiceModes = map[string]string{
	base.ToolChoiceAuto:     "auto",
	base.ToolChoiceNone:     "none",
	base.ToolChoiceRequired: "any",
}

// WireError is the shape a provider error takes in a body or a frame. The four
// providers disagree on which of these fields they populate, so one struct
// reads all of them.
type WireError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Failure renders a provider error as the terminal piece. A backend that says
// no in the body has still said no.
func Failure(err *WireError) base.Piece {
	code := err.Code
	if code == "" {
		code = err.Type
	}
	return base.FailurePiece(code, err.Message)
}

// Outcome maps a provider's terminal reason through a dialect's table. A value
// the table does not name is not dragoman's business: it completes rather than
// crashing a decoder, because that set belongs to the provider.
func Outcome(reason string, table map[string]string) base.Piece {
	if detail, named := table[reason]; named {
		return base.OutcomePiece(base.StatusIncomplete, &base.IncompleteDetails{Reason: detail}, nil)
	}
	return base.OutcomePiece(base.StatusCompleted, nil, nil)
}

// RawField returns one top-level field's bytes, for the decoders that diff a
// sub-shape against the bytes it was decoded from.
func RawField(data []byte, name string) json.RawMessage {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil
	}
	return envelope[name]
}

// RawElements returns each element of one top-level list field, unread.
func RawElements(data []byte, name string) []json.RawMessage {
	var elements []json.RawMessage
	if err := json.Unmarshal(RawField(data, name), &elements); err != nil {
		return nil
	}
	return elements
}

// JoinText folds a second run of text onto an existing one, for the dialects
// that keep instructions or a turn's prose in a single field.
func JoinText(existing, addition string) string {
	if existing == "" {
		return addition
	}
	if addition == "" {
		return existing
	}
	return existing + "\n\n" + addition
}
