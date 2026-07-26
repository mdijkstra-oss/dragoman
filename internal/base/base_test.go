package base

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestItemRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		wire string
	}{
		{
			name: "user message",
			wire: `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`,
		},
		{
			name: "assistant message",
			wire: `{"type":"message","id":"msg_1","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`,
		},
		{
			name: "function call carries both ids",
			wire: `{"type":"function_call","id":"fc_1","status":"completed","call_id":"call_9","name":"get_weather","arguments":"{\"a\":1}"}`,
		},
		{
			name: "function call output",
			wire: `{"type":"function_call_output","call_id":"call_9","output":"27C"}`,
		},
		{
			name: "reasoning keeps its pocket",
			wire: `{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"thought"}],"content":[],"encrypted_content":"blob"}`,
		},
		{
			name: "call bound state survives",
			wire: `{"type":"function_call","call_id":"c","name":"f","arguments":"{}","encrypted_content":"sig"}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			var item Item
			if err := json.Unmarshal([]byte(testCase.wire), &item); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			encoded, err := json.Marshal(item)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !sameJSON(t, encoded, []byte(testCase.wire)) {
				t.Errorf("round trip changed the item\n got: %s\nwant: %s", encoded, testCase.wire)
			}
		})
	}
}

func TestItemAcceptsShorthandContent(t *testing.T) {
	var item Item
	if err := json.Unmarshal([]byte(`{"role":"user","content":"hi"}`), &item); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if item.Type != ItemMessage {
		t.Errorf("type = %q, want %q", item.Type, ItemMessage)
	}
	if GetText(item) != "hi" {
		t.Errorf("text = %q, want %q", GetText(item), "hi")
	}
}

func TestDiffReportsUnconsumedPaths(t *testing.T) {
	type consumer struct {
		Model string `json:"model"`
		Input []struct {
			Text string `json:"text"`
		} `json:"input"`
	}
	cases := []struct {
		name  string
		raw   string
		typed any
		want  []string
	}{
		{
			name:  "nothing lost",
			raw:   `{"model":"m"}`,
			typed: consumer{Model: "m"},
			want:  nil,
		},
		{
			name:  "top level field",
			raw:   `{"model":"m","temperature":0.7}`,
			typed: consumer{Model: "m"},
			want:  []string{"temperature"},
		},
		{
			name: "path not name",
			raw:  `{"model":"m","input":[{"text":"a"},{"text":"b","signature":"s"}]}`,
			typed: consumer{Model: "m", Input: []struct {
				Text string `json:"text"`
			}{{Text: "a"}, {Text: "b"}}},
			want: []string{"input[1].signature"},
		},
		{
			name:  "empty values carry nothing",
			raw:   `{"model":"m","note":"","other":null}`,
			typed: consumer{Model: "m"},
			want:  nil,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			drops, err := Diff(json.RawMessage(testCase.raw), testCase.typed)
			if err != nil {
				t.Fatalf("diff: %v", err)
			}
			paths := make([]string, 0, len(drops))
			for _, drop := range drops {
				paths = append(paths, drop.Path)
			}
			if len(paths) == 0 {
				paths = nil
			}
			if !reflect.DeepEqual(paths, testCase.want) {
				t.Errorf("paths = %v, want %v", paths, testCase.want)
			}
		})
	}
}

func TestClassifyDefaultsToUnimplemented(t *testing.T) {
	table := map[string]DropReason{"temperature": DropUnsupported}
	classified := Classify([]Drop{{Path: "temperature"}, {Path: "input[0].tone"}}, table)
	if classified[0].Reason != DropUnsupported {
		t.Errorf("temperature = %q, want %q", classified[0].Reason, DropUnsupported)
	}
	if classified[1].Reason != DropUnimplemented {
		t.Errorf("tone = %q, want %q; the backlog has to stay visible", classified[1].Reason, DropUnimplemented)
	}
}

func TestPatchIsByteIdenticalWithoutChanges(t *testing.T) {
	raw := json.RawMessage(`{"b":1,"a":2,"nested":{"z":true}}`)
	patched, err := Patch(raw, nil)
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if string(patched) != string(raw) {
		t.Errorf("patch rewrote an unchanged body\n got: %s\nwant: %s", patched, raw)
	}
}

// TestFoldEqualsSnapshot is the invariant both output paths rest on: whatever
// the emitter accumulated, the terminal event carries. Buffered translation is
// the fold of the same event sequence, so this is what makes "folding a
// translated event stream equals the translated buffered response" hold by
// construction rather than by agreement.
func TestFoldEqualsSnapshot(t *testing.T) {
	cases := []struct {
		name   string
		pieces []Piece
	}{
		{
			name: "text only",
			pieces: []Piece{
				Start("r1", "m", 7),
				OpenPiece("a", ItemMessage), TextPiece("a", "hello"), ClosePiece("a"),
			},
		},
		{
			name: "reasoning then call",
			pieces: []Piece{
				Start("r2", "m", 7),
				OpenPiece("t", ItemReasoning), SummaryPiece("t", "why"), StatePiece("t", "blob"), ClosePiece("t"),
				OpenPiece("c", ItemFunctionCall), CallPiece("c", "call_1", "f"), ArgsPiece("c", `{"x":1}`), ClosePiece("c"),
			},
		},
		{
			name: "interleaved calls keep wire order",
			pieces: []Piece{
				Start("r3", "m", 7),
				OpenPiece("c0", ItemFunctionCall), OpenPiece("c1", ItemFunctionCall),
				ArgsPiece("c1", `{"b":2}`), ArgsPiece("c0", `{"a":1}`),
				CallPiece("c0", "call_0", "f0"), CallPiece("c1", "call_1", "f1"),
				ClosePiece("c1"), ClosePiece("c0"),
			},
		},
		{
			name: "stream ends with items open",
			pieces: []Piece{
				Start("r4", "m", 7),
				OpenPiece("a", ItemMessage), TextPiece("a", "half"),
				OutcomePiece(StatusIncomplete, &IncompleteDetails{Reason: "max_output_tokens"}, nil),
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			emitter, events := ApplyAll(Emitter{}, testCase.pieces)
			finished, terminal := Finish(emitter)
			events = append(events, terminal...)

			folded, err := Fold(events)
			if err != nil {
				t.Fatalf("fold: %v", err)
			}
			status := finished.Status
			if status == "" {
				status = StatusCompleted
			}
			want := Snapshot(finished, status)
			if !reflect.DeepEqual(folded, want) {
				t.Errorf("fold != snapshot\n got: %+v\nwant: %+v", folded, want)
			}
			assertDenseSequence(t, events)
		})
	}
}

func TestFinishClosesOpenItems(t *testing.T) {
	emitter, _ := ApplyAll(Emitter{}, []Piece{
		Start("r", "m", 0),
		OpenPiece("a", ItemMessage), TextPiece("a", "partial"),
	})
	_, events := Finish(emitter)
	folded, err := Fold(events)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if len(folded.Output) != 1 {
		t.Fatalf("output = %d items, want 1; a truncated stream must still flush", len(folded.Output))
	}
	if GetText(folded.Output[0]) != "partial" {
		t.Errorf("text = %q, want %q", GetText(folded.Output[0]), "partial")
	}
}

func TestOpenIdentifiedKeepsProviderItemID(t *testing.T) {
	emitter, _ := ApplyAll(Emitter{}, []Piece{
		Start("r", "m", 0),
		OpenIdentified("k", ItemReasoning, "rs_provider"),
		StatePiece("k", "blob"),
		ClosePiece("k"),
	})
	_, events := Finish(emitter)
	folded, _ := Fold(events)
	if folded.Output[0].ID != "rs_provider" {
		t.Errorf("id = %q, want %q; provider state is verified against the id the provider issued",
			folded.Output[0].ID, "rs_provider")
	}
}

func TestWithMaxTokensOnlyInventsWhenRequired(t *testing.T) {
	fallback := 20000
	requested := 512
	cases := []struct {
		name      string
		quirks    Quirks
		requested *int
		want      *int
		invented  bool
	}{
		{name: "absent stays absent", quirks: Quirks{}, requested: nil, want: nil},
		{name: "request wins", quirks: Quirks{RequiresMaxTokens: true, MaxTokensFallback: &fallback}, requested: &requested, want: &requested},
		{name: "fallback fires only when required", quirks: Quirks{RequiresMaxTokens: true, MaxTokensFallback: &fallback}, requested: nil, want: &fallback, invented: true},
		{name: "no fallback for the others", quirks: Quirks{MaxTokensFallback: &fallback}, requested: nil, want: nil},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, invented := WithMaxTokens(testCase.quirks, testCase.requested)
			if !reflect.DeepEqual(got, testCase.want) {
				t.Errorf("cap = %v, want %v", derefInt(got), derefInt(testCase.want))
			}
			if invented != testCase.invented {
				t.Errorf("invented = %v, want %v", invented, testCase.invented)
			}
		})
	}
}

func assertDenseSequence(t *testing.T, events []Event) {
	t.Helper()
	for index, event := range events {
		if event.SequenceNumber != index+1 {
			t.Errorf("event %d has sequence_number %d, want %d", index, event.SequenceNumber, index+1)
			return
		}
	}
}

func sameJSON(t *testing.T, left, right []byte) bool {
	t.Helper()
	var a, b any
	if err := json.Unmarshal(left, &a); err != nil {
		t.Fatalf("left: %v", err)
	}
	if err := json.Unmarshal(right, &b); err != nil {
		t.Fatalf("right: %v", err)
	}
	return reflect.DeepEqual(a, b)
}

func derefInt(value *int) any {
	if value == nil {
		return nil
	}
	return *value
}
