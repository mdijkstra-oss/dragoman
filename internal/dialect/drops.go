package dialect

import (
	"encoding/json"
	"fmt"

	"github.com/mdijkstra-oss/dragoman/internal/base"
)

// Current reasoning models reject or ignore the sampling parameters, token
// probabilities pin a caller to one model, and logit bias is meaningless across
// providers because token ids are tokenizer-specific.
var outOfScope = []string{
	"temperature", "top_p", "top_k", "seed",
	"frequency_penalty", "presence_penalty", "logit_bias",
	"logprobs", "top_logprobs", "n",
}

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

// Anything beyond the four item types belongs to a capability out of scope.
func Unmapped(path, kind, name string) base.Drop {
	return base.Drop{
		Path:   path,
		Reason: base.DropUnimplemented,
		Detail: kind + " " + name + " is not carried by the base",
	}
}

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

// Three of the four dialects share this spelling for the modes; the named form
// differs in all four.
var ChoiceModes = map[string]string{
	base.ToolChoiceAuto:     "auto",
	base.ToolChoiceNone:     "none",
	base.ToolChoiceRequired: "any",
}

// The four providers disagree about which of these fields they populate.
type WireError struct {
	Type    string `json:"type"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func Failure(err *WireError) base.Piece {
	code := err.Code
	if code == "" {
		code = err.Type
	}
	return base.FailurePiece(code, err.Message)
}

// The set of terminal reasons belongs to the provider, so an unnamed one
// completes rather than crashing the decoder.
func Outcome(reason string, table map[string]string) base.Piece {
	if detail, named := table[reason]; named {
		return base.OutcomePiece(base.StatusIncomplete, &base.IncompleteDetails{Reason: detail}, nil)
	}
	return base.OutcomePiece(base.StatusCompleted, nil, nil)
}

func RawField(data []byte, name string) json.RawMessage {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil
	}
	return envelope[name]
}

func RawElements(data []byte, name string) []json.RawMessage {
	var elements []json.RawMessage
	if err := json.Unmarshal(RawField(data, name), &elements); err != nil {
		return nil
	}
	return elements
}

// A dialect keeping two runs as separate parts must materialise this gap rather
// than lose the blank line the model reads.
const TextGap = "\n\n"

func JoinText(existing, addition string) string {
	if existing == "" {
		return addition
	}
	if addition == "" {
		return existing
	}
	return existing + TextGap + addition
}
