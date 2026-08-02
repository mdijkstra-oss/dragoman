package core_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mdijkstra-oss/dragoman/internal/base"
	"github.com/mdijkstra-oss/dragoman/internal/catalog"
	"github.com/mdijkstra-oss/dragoman/internal/core"
	"github.com/mdijkstra-oss/dragoman/internal/dialect"
)

// recorded is a real executor backed by responses captured from the live
// backends, so the decoders are exercised against bytes the providers actually
// sent rather than against bytes a test author imagined.
type recorded struct{ dir string }

func (r recorded) Do(_ context.Context, _ core.Call) (json.RawMessage, error) {
	return os.ReadFile(filepath.Join(r.dir, "buffered.json"))
}

func (r recorded) Stream(_ context.Context, _ core.Call, onFrame func(base.Frame) error) error {
	body, err := os.ReadFile(filepath.Join(r.dir, "stream.sse"))
	if err != nil {
		return err
	}
	return core.ReadFrames(bytes.NewReader(body), onFrame)
}

func fixture(name string) recorded {
	return recorded{dir: filepath.Join("..", "..", "testdata", "fixtures", name)}
}

// backends names the recorded fixtures. Each is one live call per backend of
// the same request the live matrix uses.
var backends = []string{"openai", "anthropic", "gemini", "deepseek"}

// credentialFor gives the run the environment variable the backend expects.
// Credentials come from the environment only, so a run without one is a usage
// failure even when the executor would never reach the network.
func credentialFor(t *testing.T, preset string) {
	t.Helper()
	t.Setenv(catalog.Presets()[preset].KeyEnv, "test-key")
}

func decode(t *testing.T, preset string, streamed bool) base.Response {
	t.Helper()
	credentialFor(t, preset)
	body := weatherRequest(t)
	opts := core.Options{Preset: preset, Stream: &streamed}

	events := []base.Event{}
	sink := func(event base.Event) error {
		events = append(events, event)
		return nil
	}
	result, err := core.Run(context.Background(), catalog.New(), opts, body, fixture(preset), sink)
	if err != nil {
		t.Fatalf("%s: %v", preset, err)
	}
	if !streamed {
		if result.Response == nil {
			t.Fatalf("%s: buffered run returned no response", preset)
		}
		return *result.Response
	}
	assertEventGrammar(t, events)
	folded, err := base.Fold(events)
	if err != nil {
		t.Fatalf("%s: fold: %v", preset, err)
	}
	return folded
}

// TestEveryBackendDecodesToTheBase is the offline half of the live matrix: one
// unmodified request, four dialects, the same answer shape out.
func TestEveryBackendDecodesToTheBase(t *testing.T) {
	for _, preset := range backends {
		for _, streamed := range []bool{false, true} {
			mode := "buffered"
			if streamed {
				mode = "streamed"
			}
			t.Run(preset+"/"+mode, func(t *testing.T) {
				resp := decode(t, preset, streamed)
				if resp.Status != base.StatusCompleted {
					t.Errorf("status = %q, want %q", resp.Status, base.StatusCompleted)
				}
				calls := base.FindCalls(resp)
				if len(calls) != 1 {
					t.Fatalf("calls = %d, want 1", len(calls))
				}
				if calls[0].Name != "get_weather" {
					t.Errorf("name = %q, want get_weather", calls[0].Name)
				}
				if calls[0].CallID == "" {
					t.Error("call has no identity; the next request's result could not reference it")
				}
				var arguments map[string]any
				if err := json.Unmarshal([]byte(calls[0].Arguments), &arguments); err != nil {
					t.Fatalf("arguments are not valid JSON: %v", err)
				}
				if location, _ := arguments["location"].(string); !strings.Contains(strings.ToLower(location), "rio") {
					t.Errorf("location = %v, want Rio de Janeiro", arguments["location"])
				}
				if arguments["unit"] != "celsius" {
					t.Errorf("unit = %v, want celsius", arguments["unit"])
				}
			})
		}
	}
}

// TestProviderStateSurvivesDecoding checks the pocket is filled. Without it a
// conversation cannot cross a turn boundary, because dragoman is stateless and
// the state only survives in the client's hands.
func TestProviderStateSurvivesDecoding(t *testing.T) {
	// gemini binds its signature to the call it arrived on rather than to a
	// separate reasoning item, which is why the pocket is a field on an item.
	carriers := map[string]func(base.Response) string{
		"openai":    firstState(base.ItemReasoning),
		"anthropic": firstState(base.ItemReasoning),
		"deepseek":  firstState(base.ItemReasoning),
		"gemini":    firstState(base.ItemFunctionCall),
	}
	for preset, carrier := range carriers {
		for _, streamed := range []bool{false, true} {
			mode := "buffered"
			if streamed {
				mode = "streamed"
			}
			t.Run(preset+"/"+mode, func(t *testing.T) {
				if state := carrier(decode(t, preset, streamed)); state == "" {
					t.Error("provider state is missing; the next turn would be rejected or would degrade")
				}
			})
		}
	}
}

// TestFoldEqualsBufferedForEveryBackend is the invariant that keeps the two
// paths honest. The fixtures are two separate live calls, so ids, wording and
// token counts legitimately differ; what must match is everything dragoman
// decides — the item sequence, the call identity fields, and the outcome.
func TestFoldEqualsBufferedForEveryBackend(t *testing.T) {
	for _, preset := range backends {
		t.Run(preset, func(t *testing.T) {
			buffered := shape(decode(t, preset, false))
			streamed := shape(decode(t, preset, true))
			if !reflect.DeepEqual(buffered, streamed) {
				t.Errorf("folding the stream did not equal the buffered translation\nbuffered: %+v\nstreamed: %+v",
					buffered, streamed)
			}
		})
	}
}

// TestFoldEqualsBufferedFromOneSource is the exact form of the invariant. The
// openai-responses terminal event carries the whole buffered body from the
// same call, so both sides can be compared field by field rather than
// structurally.
func TestFoldEqualsBufferedFromOneSource(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "openai", "stream.sse"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	embedded := json.RawMessage(nil)
	err = core.ReadFrames(bytes.NewReader(body), func(frame base.Frame) error {
		var event struct {
			Type     string          `json:"type"`
			Response json.RawMessage `json:"response"`
		}
		if json.Unmarshal(frame.Data, &event) == nil && event.Type == string(base.EventCompleted) {
			embedded = event.Response
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read frames: %v", err)
	}
	if embedded == nil {
		t.Fatal("stream fixture carries no terminal response")
	}

	streamed := decode(t, "openai", true)
	fromBody, err := decodeBody(embedded)
	if err != nil {
		t.Fatalf("decode embedded body: %v", err)
	}
	// The provider re-encrypts its reasoning pocket between the item frame and
	// the terminal frame, so the two ciphertexts differ by design. Every other
	// field must be identical.
	normaliseState(&streamed)
	normaliseState(&fromBody)
	if !reflect.DeepEqual(streamed, fromBody) {
		t.Errorf("fold != buffered for one source\nstream: %+v\nbody:   %+v", streamed, fromBody)
	}
}

// TestTruncatedStreamIsFlushed covers a stream that ends with items still
// open. Without an explicit flush that is undefined behaviour.
func TestTruncatedStreamIsFlushed(t *testing.T) {
	full, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "anthropic", "stream.sse"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	frames := [][]byte{}
	if err := core.ReadFrames(bytes.NewReader(full), func(frame base.Frame) error {
		frames = append(frames, append([]byte(nil), frame.Data...))
		return nil
	}); err != nil {
		t.Fatalf("read frames: %v", err)
	}
	if len(frames) < 6 {
		t.Fatalf("fixture too short: %d frames", len(frames))
	}

	spoken := catalog.Dialects()["anthropic-messages"]
	decoder := dialect.Decoder(spoken, nil)
	events := []base.Event{}
	for _, data := range frames[:len(frames)-4] {
		produced, err := decoder.Step(base.Frame{Data: data})
		if err != nil {
			t.Fatalf("step: %v", err)
		}
		events = append(events, produced...)
	}
	flushed, err := decoder.Flush()
	if err != nil {
		t.Fatalf("flush: %v", err)
	}
	events = append(events, flushed...)

	folded, err := base.Fold(events)
	if err != nil {
		t.Fatalf("a truncated stream produced no terminal response: %v", err)
	}
	for _, item := range folded.Output {
		if item.Type == base.ItemFunctionCall && item.Arguments == "" {
			continue
		}
		if item.Status == base.StatusInProgress {
			t.Errorf("item %s was emitted still open", item.ID)
		}
	}
	assertEventGrammar(t, events)
}

// TestUnknownStopReasonPassesThrough covers a value dragoman does not own. A
// provider's stop reason is the provider's business and must not crash a
// decoder.
func TestUnknownStopReasonPassesThrough(t *testing.T) {
	cases := []struct {
		name    string
		dialect string
		body    string
	}{
		{
			name:    "completions",
			dialect: "openai-completions",
			body: `{"id":"c1","model":"m","created":1,"choices":[{"index":0,` +
				`"message":{"role":"assistant","content":"hi"},"finish_reason":"insufficient_system_resource"}]}`,
		},
		{
			name:    "anthropic",
			dialect: "anthropic-messages",
			body:    `{"id":"m1","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"some_new_reason"}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			spoken := catalog.Dialects()[testCase.dialect]
			events, _, err := dialect.DecodeBody(spoken, []byte(testCase.body))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			folded, err := base.Fold(events)
			if err != nil {
				t.Fatalf("fold: %v", err)
			}
			if folded.Status != base.StatusCompleted {
				t.Errorf("status = %q, want %q", folded.Status, base.StatusCompleted)
			}
		})
	}
}

// TestBackendRefusalIsExitOne separates the two failure kinds: exit 1 is
// reserved for the backend saying no.
func TestBackendRefusalIsExitOne(t *testing.T) {
	credentialFor(t, "openai")
	_, err := core.Run(context.Background(), catalog.New(),
		core.Options{Preset: "openai"}, weatherRequest(t), refusing{}, nil)
	if err == nil {
		t.Fatal("want a failure")
	}
	if core.CodeOf(err) != core.ExitBackend {
		t.Errorf("exit = %d, want %d", core.CodeOf(err), core.ExitBackend)
	}
}

type refusing struct{}

func (refusing) Do(context.Context, core.Call) (json.RawMessage, error) {
	return nil, core.Fail(core.ExitBackend, "backend returned 400")
}

func (refusing) Stream(context.Context, core.Call, func(base.Frame) error) error {
	return core.Fail(core.ExitBackend, "backend returned 400")
}

// assertEventGrammar checks the emitted sequence is the complete Responses
// lifecycle a stock client reconstructs from, not the deltas alone.
func assertEventGrammar(t *testing.T, events []base.Event) {
	t.Helper()
	if len(events) == 0 {
		t.Fatal("no events")
	}
	if events[0].Type != base.EventCreated {
		t.Errorf("first event = %q, want %q", events[0].Type, base.EventCreated)
	}
	if !base.IsTerminal(events[len(events)-1]) {
		t.Errorf("last event = %q, want a terminal event", events[len(events)-1].Type)
	}
	terminals := 0
	for index, event := range events {
		if event.SequenceNumber != index+1 {
			t.Fatalf("sequence_number at %d = %d, want %d", index, event.SequenceNumber, index+1)
		}
		if base.IsTerminal(event) {
			terminals++
		}
	}
	if terminals != 1 {
		t.Errorf("terminal events = %d, want exactly 1", terminals)
	}
	open := map[string]bool{}
	for _, event := range events {
		switch event.Type {
		case base.EventItemAdded:
			if event.Item == nil || event.OutputIndex == nil {
				t.Fatal("output_item.added carries no item or index")
			}
			open[event.Item.ID] = true
		case base.EventItemDone:
			if event.Item == nil {
				t.Fatal("output_item.done carries no item")
			}
			if !open[event.Item.ID] {
				t.Errorf("item %s closed without opening", event.Item.ID)
			}
			delete(open, event.Item.ID)
		}
	}
	if len(open) != 0 {
		t.Errorf("items left open: %v", open)
	}
}

// itemShape is everything dragoman decided about an item, with the parts the
// provider varies between calls left out.
type itemShape struct {
	Type      base.ItemType
	Name      string
	Arguments map[string]any
	HasState  bool
}

func shape(resp base.Response) []itemShape {
	shapes := make([]itemShape, 0, len(resp.Output))
	for _, item := range resp.Output {
		entry := itemShape{Type: item.Type, Name: item.Name, HasState: item.EncryptedContent != ""}
		if item.Arguments != "" {
			_ = json.Unmarshal([]byte(item.Arguments), &entry.Arguments)
		}
		shapes = append(shapes, entry)
	}
	return shapes
}

func firstState(kind base.ItemType) func(base.Response) string {
	return func(resp base.Response) string {
		for _, item := range resp.Output {
			if item.Type == kind && item.EncryptedContent != "" {
				return item.EncryptedContent
			}
		}
		return ""
	}
}

func normaliseState(resp *base.Response) {
	for index, item := range resp.Output {
		if item.EncryptedContent != "" {
			resp.Output[index].EncryptedContent = "<present>"
		}
	}
}

func decodeBody(body json.RawMessage) (base.Response, error) {
	spoken := catalog.Dialects()["openai-responses"]
	events, _, err := dialect.DecodeBody(spoken, body)
	if err != nil {
		return base.Response{}, err
	}
	return base.Fold(events)
}
