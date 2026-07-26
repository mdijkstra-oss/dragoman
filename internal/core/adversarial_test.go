package core_test

import (
	"strings"
	"testing"

	"github.com/matthijn/dragoman/internal/base"
	"github.com/matthijn/dragoman/internal/catalog"
	"github.com/matthijn/dragoman/internal/dialect"
)

func decodeStream(t *testing.T, dialectName string, frames []base.Frame) ([]base.Event, []base.Drop) {
	t.Helper()
	spoken, known := catalog.Dialects()[dialectName]
	if !known {
		t.Fatalf("no dialect %q", dialectName)
	}
	collected := []base.Drop{}
	decoder := dialect.Decoder(spoken, func(drops []base.Drop) { collected = append(collected, drops...) })
	events := []base.Event{}
	for index, frame := range frames {
		produced, err := decoder.Step(frame)
		if err != nil {
			t.Fatalf("frame %d: %v", index, err)
		}
		events = append(events, produced...)
	}
	flushed, err := decoder.Flush()
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	return append(events, flushed...), collected
}

func dataFrames(payloads ...string) []base.Frame {
	frames := make([]base.Frame, 0, len(payloads))
	for _, payload := range payloads {
		frames = append(frames, base.Frame{Data: []byte(payload)})
	}
	return frames
}

// TestImplicitBoundariesAreInferred covers the one decoder that has to infer
// item structure rather than read it. A new tool call is a new index appearing,
// and the turn is closed only by the terminal chunk.
func TestImplicitBoundariesAreInferred(t *testing.T) {
	cases := []struct {
		name   string
		frames []base.Frame
		want   []base.Item
	}{
		{
			name: "two calls whose fragments interleave by index",
			frames: dataFrames(
				`{"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":null}}]}`,
				`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"f","arguments":""}}]}}]}`,
				`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"g","arguments":""}}]}}]}`,
				`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"x\":"}}]}}]}`,
				`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"y\":"}}]}}]}`,
				`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
				`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]}}]}`,
				`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`[DONE]`,
			),
			want: []base.Item{
				{Type: base.ItemFunctionCall, CallID: "call_a", Name: "f", Arguments: `{"x":1}`},
				{Type: base.ItemFunctionCall, CallID: "call_b", Name: "g", Arguments: `{"y":2}`},
			},
		},
		{
			name: "identity arriving after the arguments",
			frames: dataFrames(
				`{"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
				`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]}}]}`,
				`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_late","function":{"name":"f"}}]}}]}`,
				`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			),
			want: []base.Item{
				{Type: base.ItemFunctionCall, CallID: "call_late", Name: "f", Arguments: `{"a":1}`},
			},
		},
		{
			name: "usage before the finish, and a late chunk after DONE",
			frames: dataFrames(
				`{"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
				`{"id":"c1","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
				`[DONE]`,
				`{"id":"c1","choices":[{"index":0,"delta":{"content":" there"},"finish_reason":"stop"}]}`,
			),
			want: []base.Item{
				{Type: base.ItemMessage, Content: []base.ContentPart{{Type: base.PartOutputText, Text: "hi there"}}},
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			events, _ := decodeStream(t, "openai-completions", testCase.frames)
			assertEventGrammar(t, events)
			folded, err := base.Fold(events)
			if err != nil {
				t.Fatalf("fold: %v", err)
			}
			assertItems(t, folded.Output, testCase.want)
		})
	}
}

// TestStructuralFillerIsNotAnItem covers the null and empty content that both
// endpoints of that shape use as filler. Treating it as text would put an empty
// assistant message in every answer.
func TestStructuralFillerIsNotAnItem(t *testing.T) {
	cases := map[string]string{
		"null":    `{"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"content":null}}]}`,
		"empty":   `{"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"content":""}}]}`,
		"missing": `{"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
	}
	for name, opening := range cases {
		t.Run(name, func(t *testing.T) {
			events, _ := decodeStream(t, "openai-completions", dataFrames(opening,
				`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`))
			assertEventGrammar(t, events)
			folded, err := base.Fold(events)
			if err != nil {
				t.Fatalf("fold: %v", err)
			}
			if len(folded.Output) != 0 {
				t.Errorf("output = %d items, want none: filler is not content", len(folded.Output))
			}
		})
	}
}

// TestUnmappedItemTypesAreSkippedNotGuessed covers item and block types a
// provider is free to add. They belong to a capability that is out of scope, so
// they are reported rather than crashing a decoder or becoming an empty turn.
func TestUnmappedItemTypesAreSkippedNotGuessed(t *testing.T) {
	cases := []struct {
		name    string
		dialect string
		frames  []base.Frame
		wantIn  string
	}{
		{
			name:    "responses item type",
			dialect: "openai-responses",
			frames: dataFrames(
				`{"type":"response.created","response":{"id":"r1","model":"m","created_at":1,"status":"in_progress"}}`,
				`{"type":"response.output_item.added","output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"in_progress"}}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"web_search_call","id":"ws_1","status":"completed"}}`,
				`{"type":"response.completed","response":{"id":"r1","model":"m","created_at":1,"status":"completed","output":[]}}`,
			),
			wantIn: "web_search_call",
		},
		{
			name:    "anthropic block type",
			dialect: "anthropic-messages",
			frames: dataFrames(
				`{"type":"message_start","message":{"id":"m1","model":"m","usage":{"input_tokens":1}}}`,
				`{"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"abc"}}`,
				`{"type":"content_block_stop","index":0}`,
				`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			),
			wantIn: "redacted_thinking",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			events, drops := decodeStream(t, testCase.dialect, testCase.frames)
			assertEventGrammar(t, events)
			folded, err := base.Fold(events)
			if err != nil {
				t.Fatalf("fold: %v", err)
			}
			if len(folded.Output) != 0 {
				t.Errorf("output = %+v, want none: an unmapped type must not be guessed into an item", folded.Output)
			}
			if !mentions(drops, testCase.wantIn) {
				t.Errorf("drops = %+v, want one naming %q", drops, testCase.wantIn)
			}
		})
	}
}

// TestStepsLeftOpenAreFlushed covers a stream whose steps never close. The
// terminal event arrives regardless, and nothing is lost.
func TestStepsLeftOpenAreFlushed(t *testing.T) {
	frames := []base.Frame{
		{Event: "interaction.created", Data: []byte(`{"interaction":{"id":"i1","status":"in_progress","model":"g"},"event_type":"interaction.created"}`)},
		{Event: "interaction.status_update", Data: []byte(`{"interaction_id":"i1","status":"in_progress","event_type":"interaction.status_update"}`)},
		{Event: "step.start", Data: []byte(`{"index":0,"step":{"id":"s0","type":"model_output"},"event_type":"step.start"}`)},
		{Event: "step.delta", Data: []byte(`{"index":0,"delta":{"type":"text","text":"hello"},"event_type":"step.delta"}`)},
		{Event: "step.start", Data: []byte(`{"index":1,"step":{"id":"s1","type":"function_call","name":"f","signature":"sig"},"event_type":"step.start"}`)},
		{Event: "step.delta", Data: []byte(`{"index":1,"delta":{"type":"arguments_delta","arguments":"{\"a\":1}"},"event_type":"step.delta"}`)},
		{Event: "interaction.completed", Data: []byte(`{"interaction":{"id":"i1","status":"completed","model":"g"},"event_type":"interaction.completed"}`)},
	}
	events, _ := decodeStream(t, "gemini-interactions", frames)
	assertEventGrammar(t, events)
	folded, err := base.Fold(events)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	assertItems(t, folded.Output, []base.Item{
		{Type: base.ItemMessage, Content: []base.ContentPart{{Type: base.PartOutputText, Text: "hello"}}},
		{Type: base.ItemFunctionCall, CallID: "s1", Name: "f", Arguments: `{"a":1}`, EncryptedContent: "sig"},
	})
}

// TestEmptyStreamStillOpensTheLifecycle covers a backend that sent nothing. A
// terminal event with no start would be a grammar a stock client cannot read.
func TestEmptyStreamStillOpensTheLifecycle(t *testing.T) {
	for _, name := range catalog.DialectNames() {
		t.Run(name, func(t *testing.T) {
			events, _ := decodeStream(t, name, nil)
			assertEventGrammar(t, events)
			if len(events) != 3 {
				t.Errorf("events = %d, want created, in_progress and a terminal", len(events))
			}
		})
	}
}

func assertItems(t *testing.T, got, want []base.Item) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("output = %d items, want %d: %+v", len(got), len(want), got)
	}
	for index, expected := range want {
		actual := got[index]
		if actual.Type != expected.Type {
			t.Errorf("item %d type = %q, want %q", index, actual.Type, expected.Type)
		}
		if actual.CallID != expected.CallID {
			t.Errorf("item %d call_id = %q, want %q", index, actual.CallID, expected.CallID)
		}
		if actual.Name != expected.Name {
			t.Errorf("item %d name = %q, want %q", index, actual.Name, expected.Name)
		}
		if actual.Arguments != expected.Arguments {
			t.Errorf("item %d arguments = %q, want %q", index, actual.Arguments, expected.Arguments)
		}
		if actual.EncryptedContent != expected.EncryptedContent {
			t.Errorf("item %d state = %q, want %q", index, actual.EncryptedContent, expected.EncryptedContent)
		}
		if base.GetText(actual) != base.GetText(expected) {
			t.Errorf("item %d text = %q, want %q", index, base.GetText(actual), base.GetText(expected))
		}
	}
}

func mentions(drops []base.Drop, needle string) bool {
	for _, drop := range drops {
		if strings.Contains(drop.Detail, needle) || strings.Contains(drop.Path, needle) {
			return true
		}
	}
	return false
}

// TestLifecycleOpensBeforeAnyItem covers a dialect that delivers content before
// it names the response. An item announced before response.created is a grammar
// a stock client cannot read.
func TestLifecycleOpensBeforeAnyItem(t *testing.T) {
	events, _ := decodeStream(t, "openai-completions", dataFrames(
		`{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"}}]}`,
		`{"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"content":" there"},"finish_reason":"stop"}]}`,
	))
	assertEventGrammar(t, events)
	folded, err := base.Fold(events)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if folded.ID != "c1" {
		t.Errorf("id = %q, want %q once the naming chunk arrived", folded.ID, "c1")
	}
	assertItems(t, folded.Output, []base.Item{
		{Type: base.ItemMessage, Content: []base.ContentPart{{Type: base.PartOutputText, Text: "hi there"}}},
	})
}

// TestFoldEqualsBufferedForOneAnswer states the invariant on a single logical
// answer rather than on two live calls: the same content delivered as a stream
// and as a body has to produce the same base response.
func TestFoldEqualsBufferedForOneAnswer(t *testing.T) {
	cases := []struct {
		name     string
		frames   []base.Frame
		buffered string
	}{
		{
			// The order both endpoints of this shape produce: text first, then
			// the call it announces.
			name: "text then a call",
			frames: dataFrames(
				`{"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"role":"assistant","content":"done"}}]}`,
				`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"f","arguments":"{}"}}]}}]}`,
				`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			),
			buffered: `{"id":"c1","model":"m","created":1,"choices":[{"index":0,"message":{"role":"assistant","content":"done",` +
				`"tool_calls":[{"id":"call_a","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
		},
		{
			// Reasoning precedes the answer on the wire, which is also the
			// order a buffered body is rendered in.
			name: "reasoning then text",
			frames: dataFrames(
				`{"id":"c1","model":"m","created":1,"choices":[{"index":0,"delta":{"role":"assistant","reasoning_content":"because"}}]}`,
				`{"id":"c1","choices":[{"index":0,"delta":{"content":"hello"}}]}`,
				`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			),
			buffered: `{"id":"c1","model":"m","created":1,"choices":[{"index":0,"message":{"role":"assistant",` +
				`"content":"hello","reasoning_content":"because"},"finish_reason":"stop"}]}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			streamEvents, _ := decodeStream(t, "openai-completions", testCase.frames)
			streamed, err := base.Fold(streamEvents)
			if err != nil {
				t.Fatalf("fold stream: %v", err)
			}
			events, _, err := dialect.DecodeBody(catalog.Dialects()["openai-completions"], []byte(testCase.buffered))
			if err != nil {
				t.Fatalf("decode body: %v", err)
			}
			buffered, err := base.Fold(events)
			if err != nil {
				t.Fatalf("fold body: %v", err)
			}
			assertItems(t, streamed.Output, buffered.Output)
		})
	}
}
