package base

import "encoding/json"

// EventType is the one output event vocabulary. dragoman does not invent an
// event grammar, so there is no output format to choose between — only whether
// this grammar is delivered folded or unfolded.
type EventType string

const (
	EventCreated    EventType = "response.created"
	EventInProgress EventType = "response.in_progress"

	EventItemAdded EventType = "response.output_item.added"
	EventItemDone  EventType = "response.output_item.done"

	EventPartAdded EventType = "response.content_part.added"
	EventPartDone  EventType = "response.content_part.done"

	EventTextDelta EventType = "response.output_text.delta"
	EventTextDone  EventType = "response.output_text.done"

	EventArgumentsDelta EventType = "response.function_call_arguments.delta"
	EventArgumentsDone  EventType = "response.function_call_arguments.done"

	EventSummaryPartAdded EventType = "response.reasoning_summary_part.added"
	EventSummaryPartDone  EventType = "response.reasoning_summary_part.done"
	EventSummaryTextDelta EventType = "response.reasoning_summary_text.delta"
	EventSummaryTextDone  EventType = "response.reasoning_summary_text.done"

	EventCompleted  EventType = "response.completed"
	EventIncomplete EventType = "response.incomplete"
	EventFailed     EventType = "response.failed"
)

// Event is one frame of the Responses lifecycle. The bookkeeping fields
// (SequenceNumber, ItemID, OutputIndex, ContentIndex) are minted by the
// emitter, never translated, because the source dialects do not carry them.
type Event struct {
	Type           EventType `json:"type"`
	SequenceNumber int       `json:"sequence_number"`

	ItemID       string `json:"item_id,omitempty"`
	OutputIndex  *int   `json:"output_index,omitempty"`
	ContentIndex *int   `json:"content_index,omitempty"`

	Item     *Item        `json:"item,omitempty"`
	Part     *ContentPart `json:"part,omitempty"`
	Response *Response    `json:"response,omitempty"`

	Delta     string `json:"delta,omitempty"`
	Text      string `json:"text,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

// IsTerminal reports whether an event ends the response lifecycle.
func IsTerminal(event Event) bool {
	return terminalEvents[event.Type]
}

// EncodeEvent renders one event as a single line of NDJSON. Framing belongs to
// the transport; this is the payload an SSE re-framing would carry verbatim.
func EncodeEvent(event Event) ([]byte, error) { return json.Marshal(event) }

var terminalEvents = map[EventType]bool{
	EventCompleted:  true,
	EventIncomplete: true,
	EventFailed:     true,
}
