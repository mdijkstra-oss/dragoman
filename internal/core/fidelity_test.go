package core_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mdijkstra-oss/dragoman/internal/base"
	"github.com/mdijkstra-oss/dragoman/internal/catalog"
	"github.com/mdijkstra-oss/dragoman/internal/core"
)

// nonBase names the dialects that build a fresh body from typed fields, as
// opposed to the one that forwards the caller's bytes.
var nonBase = []string{"anthropic", "gemini", "deepseek"}

const oneUserTurn = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`

// TestInputAcceptsTheStringForm covers the shortest valid Responses body. The
// base is the same format as the wire, so both spellings of input arrive.
func TestInputAcceptsTheStringForm(t *testing.T) {
	body := []byte(`{"model":"m","input":"Say hi.","max_output_tokens":10}`)
	result, err := core.Run(context.Background(), catalog.New(),
		core.Options{Preset: "openai", NoExecute: true}, body, nil, nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if string(result.Sent) != string(body) {
		t.Errorf("body was rewritten\n got: %s\nwant: %s", result.Sent, body)
	}
	for _, preset := range nonBase {
		t.Run(preset, func(t *testing.T) {
			sent, _ := translate(t, core.Options{Preset: preset, MaxTokens: capOf(10)}, body)
			if !strings.Contains(mustJSON(t, sent), "Say hi.") {
				t.Errorf("prompt did not survive: %v", sent)
			}
		})
	}
}

// TestUnknownInputItemIsReportedNotFatal covers an item type the caller may
// legitimately send. It is the caller's value, not a set dragoman owns, so it
// is reported rather than crashing the encoder.
func TestUnknownInputItemIsReportedNotFatal(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"item_reference","id":"msg_1"}]}`)
	for _, preset := range nonBase {
		t.Run(preset, func(t *testing.T) {
			_, drops := translate(t, core.Options{Preset: preset, MaxTokens: capOf(10)}, body)
			if !hasDropPath(drops, "input[0]") {
				t.Errorf("drops = %+v, want one at input[0]", drops)
			}
		})
	}
}

// TestConsecutiveAssistantItemsAreKept covers a run of assistant-side items
// collapsing into one turn. Keeping only the last would lose the caller's
// history with nothing reported.
func TestConsecutiveAssistantItemsAreKept(t *testing.T) {
	body := []byte(`{"model":"m","input":[` + oneUserTurn + `,
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"First."}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Second."}]}]}`)
	for _, preset := range nonBase {
		t.Run(preset, func(t *testing.T) {
			sent, _ := translate(t, core.Options{Preset: preset, MaxTokens: capOf(10)}, body)
			rendered := mustJSON(t, sent)
			for _, kept := range []string{"First.", "Second."} {
				if !strings.Contains(rendered, kept) {
					t.Errorf("%q was lost: %s", kept, rendered)
				}
			}
		})
	}
}

// TestToolOutputSurvivesControlCharacters covers a tool result carrying the
// bytes real command output carries. Encoding it as anything but JSON makes
// the whole request unsendable.
func TestToolOutputSurvivesControlCharacters(t *testing.T) {
	const output = "ok[31mred[0m\v done"
	encoded, err := json.Marshal(output)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	body := []byte(`{"model":"m","input":[` + oneUserTurn + `,
		{"type":"function_call","call_id":"c","name":"f","arguments":"{}"},
		{"type":"function_call_output","call_id":"c","output":` + string(encoded) + `}]}`)
	for _, preset := range nonBase {
		t.Run(preset, func(t *testing.T) {
			sent, _ := translate(t, core.Options{Preset: preset, MaxTokens: capOf(10)}, body)
			rendered := mustJSON(t, sent)
			if !json.Valid([]byte(rendered)) {
				t.Fatalf("body is not valid JSON: %s", rendered)
			}
			if !strings.Contains(rendered, "red") {
				t.Errorf("tool output did not survive: %s", rendered)
			}
		})
	}
}

// TestFieldsNoDialectCarriesAreReported is the spec's sharpest rule applied to
// the encode edge: a field the base models and the caller sent, that the
// dialect never places anywhere, is a drop. Silence is not an option.
func TestFieldsNoDialectCarriesAreReported(t *testing.T) {
	body := []byte(`{"model":"m","input":[` + oneUserTurn + `],
		"reasoning":{"summary":"detailed"},"text":{"verbosity":"low"},
		"store":false,"include":["x"],"metadata":{"a":"b"}}`)
	want := []string{"text.verbosity", "reasoning.summary", "store", "include", "metadata"}
	for _, preset := range nonBase {
		t.Run(preset, func(t *testing.T) {
			_, drops := translate(t, core.Options{Preset: preset, MaxTokens: capOf(10)}, body)
			for _, path := range want {
				if !hasDropPath(drops, path) {
					t.Errorf("%s went unreported; drops = %+v", path, drops)
				}
			}
		})
	}
}

// TestHostedToolsAreDroppedNotMangled covers a tool naming a provider
// capability. Encoding it as a nameless function takes the caller's real tools
// down with it.
func TestHostedToolsAreDroppedNotMangled(t *testing.T) {
	body := []byte(`{"model":"m","input":[` + oneUserTurn + `],
		"tools":[{"type":"function","name":"get_weather"},{"type":"web_search"}]}`)
	for _, preset := range nonBase {
		t.Run(preset, func(t *testing.T) {
			sent, drops := translate(t, core.Options{Preset: preset, MaxTokens: capOf(10)}, body)
			if !hasDropPath(drops, "tools[1]") {
				t.Errorf("hosted tool went unreported; drops = %+v", drops)
			}
			rendered := mustJSON(t, sent)
			if !strings.Contains(rendered, "get_weather") {
				t.Errorf("the function tool was lost with it: %s", rendered)
			}
			if strings.Contains(rendered, `"name":""`) {
				t.Errorf("a nameless tool was emitted: %s", rendered)
			}
		})
	}
}

// TestUnmodelledToolChoiceIsReported covers a tool_choice form the base does
// not model. Inventing a wire value from it either fails loudly at the
// provider or, worse, silently gives the caller free choice.
func TestUnmodelledToolChoiceIsReported(t *testing.T) {
	const choice = `{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"gw"}]}`
	body := []byte(`{"model":"m","input":[` + oneUserTurn + `],
		"tools":[{"type":"function","name":"gw"}],"tool_choice":` + choice + `}`)

	for _, preset := range nonBase {
		t.Run(preset, func(t *testing.T) {
			sent, drops := translate(t, core.Options{Preset: preset, MaxTokens: capOf(10)}, body)
			if !hasDropPath(drops, "tool_choice") {
				t.Errorf("drops = %+v, want one at tool_choice", drops)
			}
			if got := lookup(sent, "tool_choice"); got != "" {
				t.Errorf("tool_choice = %q, want nothing invented", got)
			}
		})
	}

	t.Run("openai keeps it verbatim", func(t *testing.T) {
		sent, drops := translate(t, core.Options{Preset: "openai"}, body)
		if len(drops) != 0 {
			t.Errorf("drops = %+v, want none on the pass-through", drops)
		}
		if got := lookup(sent, "tool_choice.type"); got != "allowed_tools" {
			t.Errorf("tool_choice.type = %q, want %q", got, "allowed_tools")
		}
	})
}

// TestPerRequestStrictnessSurvivesAnExplicitAuto covers the dialect whose
// strictness is a routing mode. An explicit auto means the same as no choice
// at all, so it must not switch constrained decoding off.
func TestPerRequestStrictnessSurvivesAnExplicitAuto(t *testing.T) {
	const tools = `"tools":[{"type":"function","name":"gw","strict":true}]`
	cases := map[string]string{
		"no choice":     `{"model":"m","input":[` + oneUserTurn + `],` + tools + `}`,
		"explicit auto": `{"model":"m","input":[` + oneUserTurn + `],` + tools + `,"tool_choice":"auto"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			sent, _ := translate(t, core.Options{Preset: "gemini"}, []byte(body))
			if got := lookup(sent, "generation_config.tool_choice"); got != "validated" {
				t.Errorf("tool_choice = %q, want %q", got, "validated")
			}
		})
	}
}

// TestProviderErrorInsideOkIsExitOne covers a backend that says no in the body
// rather than in the status line. It is still the backend saying no, and a
// hook gating on the exit code has to see it.
func TestProviderErrorInsideOkIsExitOne(t *testing.T) {
	cases := []struct {
		name    string
		preset  string
		body    string
		message string
	}{
		{
			name:    "responses error object",
			preset:  "openai",
			body:    `{"error":{"message":"Rate limit reached","type":"rate_limit_error","code":"rate_limit"}}`,
			message: "Rate limit reached",
		},
		{
			name:    "responses failed status",
			preset:  "openai",
			body:    `{"id":"r1","model":"m","status":"failed","error":{"code":"server_error","message":"boom"},"output":[]}`,
			message: "boom",
		},
		{
			name:    "completions error object",
			preset:  "deepseek",
			body:    `{"error":{"message":"quota exceeded","type":"insufficient_quota"}}`,
			message: "quota exceeded",
		},
		{
			name:    "anthropic error body",
			preset:  "anthropic",
			body:    `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
			message: "Overloaded",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			credentialFor(t, testCase.preset)
			buffered := false
			_, err := core.Run(context.Background(), catalog.New(),
				core.Options{Preset: testCase.preset, Stream: &buffered},
				weatherRequest(t), replying(testCase.body), nil)
			if err == nil {
				t.Fatal("a provider error was reported as success")
			}
			if core.CodeOf(err) != core.ExitBackend {
				t.Errorf("exit = %d, want %d", core.CodeOf(err), core.ExitBackend)
			}
			if !strings.Contains(err.Error(), testCase.message) {
				t.Errorf("error does not carry the provider's words: %v", err)
			}
		})
	}
}

// replying is a real executor that answers with a fixed body, for the failure
// shapes a live backend will not produce on demand.
type replying string

func (r replying) Do(context.Context, core.Call) (json.RawMessage, error) {
	return json.RawMessage(r), nil
}

func (r replying) Stream(_ context.Context, _ core.Call, onFrame func(base.Frame) error) error {
	return onFrame(base.Frame{Data: []byte(r)})
}

func capOf(value int) *int { return &value }

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(encoded)
}

// TestUsageMeansTheSameThingEverywhere covers the one output shape having one
// counting method. Responses counts reasoning inside output_tokens and names
// the subset in the details; a backend that reports the two as siblings is
// assembled into that shape, or a caller doing arithmetic on the same field
// gets a different answer per backend.
func TestUsageMeansTheSameThingEverywhere(t *testing.T) {
	for _, preset := range backends {
		for _, streamed := range []bool{false, true} {
			mode := "buffered"
			if streamed {
				mode = "streamed"
			}
			t.Run(preset+"/"+mode, func(t *testing.T) {
				usage := decode(t, preset, streamed).Usage
				if usage == nil {
					t.Fatal("no usage reported")
				}
				if usage.InputTokens+usage.OutputTokens != usage.TotalTokens {
					t.Errorf("input %d + output %d != total %d",
						usage.InputTokens, usage.OutputTokens, usage.TotalTokens)
				}
				if usage.OutputTokenDetails == nil {
					return
				}
				if reasoning := usage.OutputTokenDetails.ReasoningTokens; reasoning > usage.OutputTokens {
					t.Errorf("reasoning %d exceeds output %d; it is a subset, not a sibling",
						reasoning, usage.OutputTokens)
				}
			})
		}
	}
}
