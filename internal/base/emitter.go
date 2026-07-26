package base

import (
	"fmt"
	"sort"
)

// Emitter mints the Responses bookkeeping the source dialects do not carry —
// item_id, output_index, content_index and sequence_number. Because there is
// one output grammar, that bookkeeping is written once here and every dialect
// decoder drives it. State is threaded, never mutated (R-9).
type Emitter struct {
	ResponseID string
	Model      string
	CreatedAt  int64

	// Started records that the lifecycle was opened, so a stream that carried
	// nothing still produces created and in_progress before its terminal
	// event rather than a terminal event on its own.
	Started bool

	Seq       int
	NextIndex int

	Open []OpenItem
	Done []PlacedItem

	Usage      *Usage
	Status     string
	Incomplete *IncompleteDetails
	Error      *ResponseError
}

// OpenItem is an item the decoder has seen open and not yet close. What must
// be held is bounded by one item: the accumulators below plus its identity.
type OpenItem struct {
	Key         string
	Kind        ItemType
	ItemID      string
	OutputIndex int

	// Parts holds the two content channels an item can carry, indexed by
	// channel. They differ only in which events name them.
	Parts [channels]partState

	Arguments string

	Name   string
	CallID string

	// State is the provider state pocket. It is filled during the item and
	// attached at output_item.done rather than .added, because Gemini streams
	// its signature as a delta and DeepSeek's reasoning arrives progressively.
	State string
}

// partState is one content channel of an open item.
type partState struct {
	Open bool
	Text string
}

// channelID names a content channel. The answer and the reasoning summary run
// through identical machinery under different event names.
type channelID int

const (
	answer channelID = iota
	summary
	channels
)

var channelEvents = [channels]struct {
	Part  string
	Added EventType
	Done  EventType
	Delta EventType
	Text  EventType
}{
	answer:  {PartOutputText, EventPartAdded, EventPartDone, EventTextDelta, EventTextDone},
	summary: {PartSummary, EventSummaryPartAdded, EventSummaryPartDone, EventSummaryTextDelta, EventSummaryTextDone},
}

// PlacedItem is a closed item together with the output index it occupies, so
// that items which interleave still fold back in wire order.
type PlacedItem struct {
	Index int
	Item  Item
}

func Begin(e Emitter) (Emitter, []Event) {
	if e.Started {
		return e, nil
	}
	e.Started = true
	e, created := emit(e, Event{Type: EventCreated, Response: snapshotPtr(e, StatusInProgress)})
	e, progress := emit(e, Event{Type: EventInProgress, Response: snapshotPtr(e, StatusInProgress)})
	return e, append(created, progress...)
}

func Open(e Emitter, key string, kind ItemType) (Emitter, []Event) {
	return OpenAs(e, key, kind, "")
}

// OpenAs starts an item and assigns it the next output index. An id supplied
// by the source dialect is kept verbatim, because a provider verifies its own
// state against the id it issued; only a dialect that carries no item identity
// gets a minted one.
func OpenAs(e Emitter, key string, kind ItemType, itemID string) (Emitter, []Event) {
	if _, found := findOpen(e, key); found {
		return e, nil
	}
	// A dialect may deliver content before it names the response — the
	// identity of a chat completion arrives on whichever chunk carries it. The
	// lifecycle opens here so an item is never announced before the response
	// it belongs to.
	e, opened := Begin(e)
	if itemID == "" {
		itemID = mintItemID(kind, e.NextIndex)
	}
	item := OpenItem{
		Key:         key,
		Kind:        kind,
		ItemID:      itemID,
		OutputIndex: e.NextIndex,
	}
	e.NextIndex++
	e.Open = appendOpen(e.Open, item)
	e, added := emit(e, Event{
		Type:        EventItemAdded,
		OutputIndex: intPtr(item.OutputIndex),
		Item:        openedItemPtr(item),
	})
	return e, append(opened, added...)
}

// TextDelta appends assistant text to a message item, opening it if the
// dialect never announced a start.
func TextDelta(e Emitter, key, text string) (Emitter, []Event) {
	return channelDelta(e, key, ItemMessage, answer, text)
}

// SummaryDelta appends to a reasoning item's human-readable summary.
func SummaryDelta(e Emitter, key, text string) (Emitter, []Event) {
	return channelDelta(e, key, ItemReasoning, summary, text)
}

// channelDelta appends to one content channel, opening its part on first use so
// that an item which never produces content never announces one.
func channelDelta(e Emitter, key string, kind ItemType, which channelID, text string) (Emitter, []Event) {
	e, opened := ensureOpen(e, key, kind)
	index, _ := findOpen(e, key)
	item := e.Open[index]
	names := channelEvents[which]

	var partEvents []Event
	if !item.Parts[which].Open {
		item.Parts[which].Open = true
		e.Open = replaceOpen(e.Open, index, item)
		e, partEvents = emit(e, Event{
			Type:         names.Added,
			ItemID:       item.ItemID,
			OutputIndex:  intPtr(item.OutputIndex),
			ContentIndex: intPtr(0),
			Part:         &ContentPart{Type: names.Part},
		})
	}

	item.Parts[which].Text += text
	e.Open = replaceOpen(e.Open, index, item)
	e, delta := emit(e, Event{
		Type:         names.Delta,
		ItemID:       item.ItemID,
		OutputIndex:  intPtr(item.OutputIndex),
		ContentIndex: intPtr(0),
		Delta:        text,
	})
	return e, concat(opened, partEvents, delta)
}

// ArgumentsDelta appends to a function call's argument buffer.
func ArgumentsDelta(e Emitter, key, fragment string) (Emitter, []Event) {
	e, opened := ensureOpen(e, key, ItemFunctionCall)
	index, _ := findOpen(e, key)
	item := e.Open[index]
	item.Arguments += fragment
	e.Open = replaceOpen(e.Open, index, item)
	e, delta := emit(e, Event{
		Type:        EventArgumentsDelta,
		ItemID:      item.ItemID,
		OutputIndex: intPtr(item.OutputIndex),
		Delta:       fragment,
	})
	return e, append(opened, delta...)
}

// SetCall records the identity of a function call. The call id is the identity
// the next request's result references; each dialect places it where it
// belongs.
func SetCall(e Emitter, key, callID, name string) (Emitter, []Event) {
	e, opened := ensureOpen(e, key, ItemFunctionCall)
	index, _ := findOpen(e, key)
	item := e.Open[index]
	if callID != "" {
		item.CallID = callID
	}
	if name != "" {
		item.Name = name
	}
	e.Open = replaceOpen(e.Open, index, item)
	return e, opened
}

// SetState fills the provider state pocket for an item. dragoman does not
// inspect it, classify it, or care that one provider's is base64 and
// another's readable prose. State accumulates because some backends stream
// their signature as a delta.
func SetState(e Emitter, key, state string) (Emitter, []Event) {
	e, opened := ensureOpen(e, key, ItemReasoning)
	index, _ := findOpen(e, key)
	item := e.Open[index]
	item.State += state
	e.Open = replaceOpen(e.Open, index, item)
	return e, opened
}

// Close finishes an item, emitting its done events and moving it into the
// output at the index it was assigned.
func Close(e Emitter, key string) (Emitter, []Event) {
	index, found := findOpen(e, key)
	if !found {
		return e, nil
	}
	item := e.Open[index]
	e.Open = removeOpen(e.Open, index)

	events := []Event{}
	for which := answer; which < channels; which++ {
		if !item.Parts[which].Open {
			continue
		}
		names := channelEvents[which]
		var textDone, partDone []Event
		e, textDone = emit(e, Event{
			Type:         names.Text,
			ItemID:       item.ItemID,
			OutputIndex:  intPtr(item.OutputIndex),
			ContentIndex: intPtr(0),
			Text:         item.Parts[which].Text,
		})
		e, partDone = emit(e, Event{
			Type:         names.Done,
			ItemID:       item.ItemID,
			OutputIndex:  intPtr(item.OutputIndex),
			ContentIndex: intPtr(0),
			Part:         &ContentPart{Type: names.Part, Text: item.Parts[which].Text},
		})
		events = concat(events, textDone, partDone)
	}
	if item.Kind == ItemFunctionCall {
		var argsDone []Event
		e, argsDone = emit(e, Event{
			Type:        EventArgumentsDone,
			ItemID:      item.ItemID,
			OutputIndex: intPtr(item.OutputIndex),
			Arguments:   item.Arguments,
		})
		events = append(events, argsDone...)
	}

	closed := closedItem(item)
	e.Done = append(append([]PlacedItem(nil), e.Done...), PlacedItem{Index: item.OutputIndex, Item: closed})

	var itemDone []Event
	e, itemDone = emit(e, Event{
		Type:        EventItemDone,
		OutputIndex: intPtr(item.OutputIndex),
		Item:        &closed,
	})
	return e, append(events, itemDone...)
}

func SetUsage(e Emitter, usage *Usage) Emitter {
	e.Usage = usage
	return e
}

// SetOutcome records the terminal state. An unknown provider stop reason is
// not dragoman's business; callers map it to one of the three base statuses
// and keep the provider's own wording in IncompleteDetails.
func SetOutcome(e Emitter, status string, incomplete *IncompleteDetails, err *ResponseError) Emitter {
	e.Status = status
	e.Incomplete = incomplete
	e.Error = err
	return e
}

// Finish closes anything still open and emits the terminal event. A stream may
// end with items open — max_tokens, a dropped connection, a mid-stream refusal
// — and without an explicit flush that is undefined behaviour.
func Finish(e Emitter) (Emitter, []Event) {
	e, events := Begin(e)
	for len(e.Open) > 0 {
		var closed []Event
		e, closed = Close(e, e.Open[0].Key)
		events = append(events, closed...)
	}
	status := e.Status
	if status == "" {
		status = StatusCompleted
	}
	eventType, known := terminalFor[status]
	if !known {
		panic("base: unknown terminal status " + status)
	}
	var terminal []Event
	e, terminal = emit(e, Event{Type: eventType, Response: snapshotPtr(e, status)})
	return e, append(events, terminal...)
}

func Snapshot(e Emitter, status string) Response {
	placed := append([]PlacedItem(nil), e.Done...)
	sort.SliceStable(placed, func(a, b int) bool { return placed[a].Index < placed[b].Index })
	output := make([]Item, 0, len(placed))
	for _, entry := range placed {
		output = append(output, entry.Item)
	}
	return Response{
		ID:                e.ResponseID,
		Object:            ObjectResponse,
		CreatedAt:         e.CreatedAt,
		Status:            status,
		Model:             e.Model,
		Output:            output,
		Usage:             e.Usage,
		IncompleteDetails: e.Incomplete,
		Error:             e.Error,
	}
}

var terminalFor = map[string]EventType{
	StatusCompleted:  EventCompleted,
	StatusIncomplete: EventIncomplete,
	StatusFailed:     EventFailed,
}

func emit(e Emitter, event Event) (Emitter, []Event) {
	e.Seq++
	event.SequenceNumber = e.Seq
	return e, []Event{event}
}

func ensureOpen(e Emitter, key string, kind ItemType) (Emitter, []Event) {
	if _, found := findOpen(e, key); found {
		return e, nil
	}
	return Open(e, key, kind)
}

func findOpen(e Emitter, key string) (int, bool) {
	for index, item := range e.Open {
		if item.Key == key {
			return index, true
		}
	}
	return 0, false
}

func appendOpen(open []OpenItem, item OpenItem) []OpenItem {
	return append(append([]OpenItem(nil), open...), item)
}

func replaceOpen(open []OpenItem, index int, item OpenItem) []OpenItem {
	next := append([]OpenItem(nil), open...)
	next[index] = item
	return next
}

func removeOpen(open []OpenItem, index int) []OpenItem {
	next := append([]OpenItem(nil), open[:index]...)
	return append(next, open[index+1:]...)
}

func openedItemPtr(item OpenItem) *Item {
	opened := Item{Type: item.Kind, ID: item.ItemID, Status: StatusInProgress}
	if item.Kind == ItemMessage {
		opened.Role = RoleAssistant
	}
	return &opened
}

func closedItem(item OpenItem) Item {
	closed := Item{Type: item.Kind, ID: item.ItemID, Status: StatusCompleted}
	switch item.Kind {
	case ItemMessage:
		closed.Role = RoleAssistant
		closed.Content = []ContentPart{{Type: PartOutputText, Text: item.Parts[answer].Text}}
	case ItemFunctionCall:
		closed.CallID = item.CallID
		closed.Name = item.Name
		closed.Arguments = item.Arguments
		// A backend that binds its state to the call rather than to a
		// preceding reasoning item keeps it here, so it goes back on the item
		// it arrived on.
		closed.EncryptedContent = item.State
	case ItemReasoning:
		closed.Status = ""
		closed.EncryptedContent = item.State
		if text := item.Parts[summary].Text; text != "" {
			closed.Summary = []ContentPart{{Type: PartSummary, Text: text}}
		}
	default:
		panic("base: cannot close item of kind " + string(item.Kind))
	}
	return closed
}

func snapshotPtr(e Emitter, status string) *Response {
	snapshot := Snapshot(e, status)
	return &snapshot
}

func mintItemID(kind ItemType, index int) string {
	prefix, known := idPrefixes[kind]
	if !known {
		panic("base: no id prefix for kind " + string(kind))
	}
	return fmt.Sprintf("%s_%d", prefix, index)
}

var idPrefixes = map[ItemType]string{
	ItemMessage:      "msg",
	ItemFunctionCall: "fc",
	ItemReasoning:    "rs",
}

func intPtr(value int) *int { return &value }

func concat(groups ...[]Event) []Event {
	total := []Event{}
	for _, group := range groups {
		total = append(total, group...)
	}
	return total
}
