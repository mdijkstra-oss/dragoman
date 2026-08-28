package core_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mdijkstra-oss/dragoman/internal/base"
	"github.com/mdijkstra-oss/dragoman/internal/catalog"
	"github.com/mdijkstra-oss/dragoman/internal/core"
)

// weatherRequest is the request the live matrix uses: one tool and a prompt
// that needs a step of reasoning before the call can be filled in.
func weatherRequest(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "weather.json"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return body
}

func translate(t *testing.T, opts core.Options, body []byte) (map[string]any, []base.Drop) {
	t.Helper()
	opts.NoExecute = true
	result, err := core.Run(context.Background(), catalog.New(), opts, body, nil, nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(result.Sent, &decoded); err != nil {
		t.Fatalf("translated body: %v", err)
	}
	return decoded, result.Drops
}

// TestRoundTripThroughBaseIsByteIdentical checks the one edge where the base
// and the wire format are the same bytes. A body that already names an output
// cap leaves nothing to fill in, so nothing is re-serialised and a Responses
// request handed to a Responses backend arrives unchanged.
func TestRoundTripThroughBaseIsByteIdentical(t *testing.T) {
	const items = `[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]`
	cases := []struct {
		name string
		body string
	}{
		{name: "stream stated", body: `{"model":"m","input":` + items + `,"max_output_tokens":512,"stream":false}`},
		// An absent stream already means false, so nothing needs writing in.
		{name: "stream absent", body: `{"model":"m","input":` + items + `,"max_output_tokens":512}`},
		{name: "key order preserved", body: `{"input":` + items + `,"instructions":"be terse","max_output_tokens":512,"model":"m"}`},
		{name: "reasoning stated", body: `{"model":"m","reasoning":{"effort":"low"},"input":` + items + `,"max_output_tokens":512}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := core.Run(context.Background(), catalog.New(),
				core.Options{Preset: "openai", NoExecute: true}, []byte(testCase.body), nil, nil)
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			if string(result.Sent) != testCase.body {
				t.Errorf("body was rewritten\n got: %s\nwant: %s", result.Sent, testCase.body)
			}
			if len(result.Drops) != 0 {
				t.Errorf("drops = %v, want none: a pass-through loses nothing", result.Drops)
			}
		})
	}
}

// TestTranslatesForEveryBackend is the --no-execute check: the whole pipeline
// minus the network, for all four dialects.
func TestTranslatesForEveryBackend(t *testing.T) {
	body := weatherRequest(t)
	cases := []struct {
		preset  string
		wants   map[string]string
		absent  []string
		toolsAt string
	}{
		{
			preset:  "openai",
			wants:   map[string]string{"reasoning.effort": "medium", "max_output_tokens": "20000"},
			toolsAt: "tools.0.name",
		},
		{
			preset:  "anthropic",
			wants:   map[string]string{"output_config.effort": "medium", "max_tokens": "20000"},
			absent:  []string{"reasoning", "reasoning_effort", "temperature"},
			toolsAt: "tools.0.name",
		},
		{
			preset:  "gemini",
			wants:   map[string]string{"generation_config.thinking_level": "medium", "generation_config.max_output_tokens": "20000"},
			absent:  []string{"reasoning", "reasoning_effort", "max_output_tokens"},
			toolsAt: "tools.0.name",
		},
		{
			preset:  "deepseek",
			wants:   map[string]string{"reasoning_effort": "medium", "max_tokens": "20000"},
			absent:  []string{"reasoning"},
			toolsAt: "tools.0.function.name",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.preset, func(t *testing.T) {
			sent, _ := translate(t, core.Options{Preset: testCase.preset}, body)
			for path, want := range testCase.wants {
				if got := lookup(sent, path); got != want {
					t.Errorf("%s = %v, want %v", path, got, want)
				}
			}
			for _, path := range testCase.absent {
				if lookup(sent, path) != "" {
					t.Errorf("%s is present, want absent", path)
				}
			}
			if got := lookup(sent, testCase.toolsAt); got != "get_weather" {
				t.Errorf("%s = %q, want %q", testCase.toolsAt, got, "get_weather")
			}
		})
	}
}

// TestEffortRelocatesButNeverChanges is the spec's central claim: the field is
// structural and dragoman maps it; the value is opaque and passes through byte
// for byte.
func TestEffortRelocatesButNeverChanges(t *testing.T) {
	body := weatherRequest(t)
	homes := map[string]string{
		"openai":    "reasoning.effort",
		"anthropic": "output_config.effort",
		"gemini":    "generation_config.thinking_level",
		"deepseek":  "reasoning_effort",
	}
	// A level no provider shares, to prove nothing is normalised, case-folded,
	// validated against a set, or mapped onto a nearer neighbour.
	const level = "XiGhLy-Bespoke"
	for preset, home := range homes {
		t.Run(preset, func(t *testing.T) {
			sent, _ := translate(t, core.Options{Preset: preset, Effort: level}, body)
			if got := lookup(sent, home); got != level {
				t.Errorf("%s = %q, want %q untouched", home, got, level)
			}
		})
	}
}

// TestAbsentStaysAbsent guards the rule that filling in a level nobody asked
// for silently overrides a provider default and changes both cost and
// behaviour.
func TestAbsentStaysAbsent(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	homes := map[string]string{
		"openai":    "reasoning",
		"anthropic": "output_config",
		"gemini":    "generation_config.thinking_level",
		"deepseek":  "reasoning_effort",
	}
	for preset, home := range homes {
		t.Run(preset, func(t *testing.T) {
			sent, _ := translate(t, core.Options{Preset: preset}, body)
			if got := lookup(sent, home); got != "" {
				t.Errorf("%s = %q, want absent", home, got)
			}
		})
	}
}

// TestOutputCapPrecedence covers the order the cap is settled in: the flag,
// then the body, then the one default every backend shares.
func TestOutputCapPrecedence(t *testing.T) {
	withCap := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"max_output_tokens":512}`)
	withoutCap := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	flagged := 99

	cases := []struct {
		name   string
		preset string
		body   []byte
		flag   *int
		path   string
		want   string
	}{
		{name: "anthropic defaults", preset: "anthropic", body: withoutCap, path: "max_tokens", want: "20000"},
		{name: "anthropic takes the body", preset: "anthropic", body: withCap, path: "max_tokens", want: "512"},
		{name: "anthropic flag wins over body and default", preset: "anthropic", body: withCap, flag: &flagged, path: "max_tokens", want: "99"},
		{name: "openai defaults", preset: "openai", body: withoutCap, path: "max_output_tokens", want: "20000"},
		{name: "gemini defaults", preset: "gemini", body: withoutCap, path: "generation_config.max_output_tokens", want: "20000"},
		{name: "deepseek defaults", preset: "deepseek", body: withoutCap, path: "max_tokens", want: "20000"},
		{name: "deepseek flag wins", preset: "deepseek", body: withoutCap, flag: &flagged, path: "max_tokens", want: "99"},
		{name: "openai-chat spells the cap differently", preset: "openai-chat", body: withoutCap, flag: &flagged, path: "max_completion_tokens", want: "99"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			sent, _ := translate(t, core.Options{Preset: testCase.preset, MaxTokens: testCase.flag}, testCase.body)
			if got := lookup(sent, testCase.path); got != testCase.want {
				t.Errorf("%s = %q, want %q", testCase.path, got, testCase.want)
			}
		})
	}
}

// TestResponsesWrapperOmitsOnlyTheSynthesizedCap covers wrappers such as
// codex-bridge that cannot control their output cap. The service quirk removes
// Dragoman's fallback without weakening the bridge's refusal of caller limits.
func TestResponsesWrapperOmitsOnlyTheSynthesizedCap(t *testing.T) {
	table := catalog.New()
	table.Presets["wrapper"] = base.Backend{
		Name: "wrapper", Dialect: "openai-responses", Endpoint: "http://127.0.0.1:1/v1",
		KeyEnv: "WRAPPER_KEY", OmitDefaultMaxOutputTokens: true,
	}
	cases := []struct {
		name string
		body string
		want string
	}{
		{name: "absent stays absent", body: `{"model":"m","input":"hi"}`, want: ""},
		{name: "caller cap remains", body: `{"model":"m","input":"hi","max_output_tokens":512}`, want: "512"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result, err := core.Run(context.Background(), table,
				core.Options{Preset: "wrapper", NoExecute: true}, []byte(testCase.body), nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			var sent map[string]any
			if err := json.Unmarshal(result.Sent, &sent); err != nil {
				t.Fatal(err)
			}
			if got := lookup(sent, "max_output_tokens"); got != testCase.want {
				t.Fatalf("max_output_tokens = %q, want %q; body: %s", got, testCase.want, result.Sent)
			}
		})
	}
}

// TestStrictRefusesToSendOnADrop covers the mode where silence is not an
// option: a dropped field produces a confident, well-formed, wrong answer.
func TestStrictRefusesToSendOnADrop(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"temperature":0.7}`)
	_, err := core.Run(context.Background(), catalog.New(),
		core.Options{Preset: "anthropic", Strict: true}, body, nil, nil)
	if err == nil {
		t.Fatal("strict accepted a droppable field")
	}
	if core.CodeOf(err) != core.ExitUsage {
		t.Errorf("exit = %d, want %d", core.CodeOf(err), core.ExitUsage)
	}
	if !strings.Contains(err.Error(), "temperature") {
		t.Errorf("error does not name the field with its path: %v", err)
	}
}

// TestDropsAreReportedWithPaths covers a field the base knows and a field
// invented by the caller: both are lost, and both are named at the path the
// caller wrote.
func TestDropsAreReportedWithPaths(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"temperature":0.7,"tone":"warm"}`)
	_, drops := translate(t, core.Options{Preset: "gemini"}, body)
	for _, path := range []string{"temperature", "tone"} {
		if !hasDropPath(drops, path) {
			t.Errorf("%s went unreported; drops = %+v", path, drops)
		}
	}
}

// TestSchemaAgainstASchemalessBackendBecomesAPrompt covers the one endpoint
// that constrains output to JSON without accepting a schema. Both halves are
// required — the mode alone yields JSON of any shape — and the caller is still
// told the schema is no longer enforced.
func TestSchemaAgainstASchemalessBackendBecomesAPrompt(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
		`"text":{"format":{"type":"json_schema","name":"p","strict":true,"schema":{"type":"object"}}}}`)

	sent, drops := translate(t, core.Options{Preset: "deepseek"}, body)
	if got := lookup(sent, "response_format.type"); got != "json_object" {
		t.Errorf("response_format.type = %q, want %q", got, "json_object")
	}
	if lookup(sent, "response_format.json_schema") != "" {
		t.Error("a schema was forwarded to a backend with no schema mode")
	}
	system := lookup(sent, "messages.0.content")
	if !strings.Contains(system, "json") {
		t.Errorf("system message does not name json, which the mode requires: %s", system)
	}
	if !strings.Contains(system, `type`) || !strings.Contains(system, `object`) {
		t.Errorf("system message does not carry the schema: %s", system)
	}
	if !hasDropPath(drops, "text.format") {
		t.Errorf("schema drop was not reported: %v", drops)
	}

	sent, _ = translate(t, core.Options{Preset: "openai-chat"}, body)
	if got := lookup(sent, "response_format.json_schema.strict"); got != "true" {
		t.Errorf("strict = %q, want nested inside json_schema", got)
	}
}

// TestSchemaAsPromptIsRefusedOffCompletions keeps the capability tied to the one
// wire format that has a JSON mode to fall back to. Only true carries a claim:
// the key omitted, or written false, says nothing and is legal everywhere, which
// is what lets most services never name it.
func TestSchemaAsPromptIsRefusedOffCompletions(t *testing.T) {
	entry := "anthropic:\n  endpoint: https://api.anthropic.com/v1\n  protocol: anthropic-messages\n  auth: ANTHROPIC_API_KEY\n"

	for _, quiet := range []string{entry, entry + "  schema_as_prompt: false\n"} {
		table, err := catalog.Load([]byte(quiet))
		if err != nil {
			t.Fatalf("table without the claim did not load: %v", err)
		}
		if table["anthropic"].SchemaAsPrompt {
			t.Error("SchemaAsPrompt is true where the table did not say so")
		}
	}

	_, err := catalog.Load([]byte(entry + "  schema_as_prompt: true\n"))
	if err == nil {
		t.Fatal("schema_as_prompt on anthropic-messages loaded, want a load failure")
	}
	if !strings.Contains(err.Error(), "schema_as_prompt") {
		t.Errorf("error does not name the field: %v", err)
	}
}

// TestAFormatLabelIsDroppedWithItsExplanation covers a loss that costs the
// caller nothing beside one that does. Two dialects carry a schema without
// carrying its name or its strict flag, and a schema they carry is enforced
// anyway — so the request that asked for strict got what it asked for, and only
// the request that asked to relax it lost something. Both are reported, and the
// sentence is what tells them apart.
func TestAFormatLabelIsDroppedWithItsExplanation(t *testing.T) {
	body := func(strict string) []byte {
		return []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
			`"text":{"format":{"type":"json_schema","name":"p","strict":` + strict + `,"schema":{"type":"object"}}}}`)
	}
	for _, preset := range []string{"gemini", "anthropic"} {
		t.Run(preset, func(t *testing.T) {
			_, enforced := translate(t, core.Options{Preset: preset}, body("true"))
			for _, path := range []string{"text.format.name", "text.format.strict"} {
				if !hasDropPath(enforced, path) {
					t.Errorf("%s went unreported; drops = %+v", path, enforced)
				}
			}
			for _, drop := range enforced {
				if drop.Detail == "" {
					t.Errorf("%s is dropped without saying what it cost: %+v", drop.Path, enforced)
				}
			}
			if mentions(enforced, "asked it not to be") {
				t.Errorf("a request that got the enforcement it asked for was told it lost it: %+v", enforced)
			}

			_, relaxed := translate(t, core.Options{Preset: preset}, body("false"))
			if !hasDropPath(relaxed, "text.format.strict") {
				t.Errorf("a request that asked to relax the schema was told nothing: %v", relaxed)
			}
			if !mentions(relaxed, "asked it not to be") {
				t.Errorf("the caller is not told the schema is enforced against the request: %+v", relaxed)
			}
		})
	}
}

// TestMixedStrictnessCannotBeExpressedPerRequest covers the named drop: one
// dialect carries strictness per request, so a mix is inexpressible.
func TestMixedStrictnessCannotBeExpressedPerRequest(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
		`"tools":[{"type":"function","name":"a","strict":true},{"type":"function","name":"b"}]}`)
	_, drops := translate(t, core.Options{Preset: "gemini"}, body)
	if !hasDropPath(drops, "tools[].strict") {
		t.Errorf("mixed strictness was not reported: %v", drops)
	}
}

// TestNamedFunctionForcingDiffersInAllFour covers the four spellings.
func TestNamedFunctionForcingDiffersInAllFour(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
		`"tools":[{"type":"function","name":"get_time"}],"tool_choice":{"type":"function","name":"get_time"}}`)
	cases := []struct {
		preset string
		path   string
		want   string
	}{
		{preset: "openai", path: "tool_choice.name", want: "get_time"},
		{preset: "anthropic", path: "tool_choice.type", want: "tool"},
		{preset: "deepseek", path: "tool_choice.function.name", want: "get_time"},
		{preset: "gemini", path: "generation_config.tool_choice.allowed_tools.mode", want: "any"},
	}
	for _, testCase := range cases {
		t.Run(testCase.preset, func(t *testing.T) {
			sent, _ := translate(t, core.Options{Preset: testCase.preset}, body)
			if got := lookup(sent, testCase.path); got != testCase.want {
				t.Errorf("%s = %q, want %q", testCase.path, got, testCase.want)
			}
		})
	}
}

// TestUnknownBackendIsAUsageFailure keeps exit 2 trustworthy: it means
// dragoman could not run, never that a backend said no.
func TestUnknownBackendIsAUsageFailure(t *testing.T) {
	cases := []struct {
		name string
		opts core.Options
	}{
		{name: "unknown dialect", opts: core.Options{BackendProtocol: "no-such-dialect", Endpoint: "http://x"}},
		{name: "unknown preset", opts: core.Options{Preset: "no-such-preset"}},
		{name: "no endpoint", opts: core.Options{BackendProtocol: "anthropic-messages"}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := core.Run(context.Background(), catalog.New(), testCase.opts, []byte(`{}`), nil, nil)
			if err == nil {
				t.Fatal("want a failure")
			}
			if core.CodeOf(err) != core.ExitUsage {
				t.Errorf("exit = %d, want %d", core.CodeOf(err), core.ExitUsage)
			}
		})
	}
}

// TestPresetsAreExpressibleInFlags keeps presets a convenience: anything a
// preset does has to be reachable from the command line, so that an endpoint
// nobody anticipated is a command line rather than a patch.
func TestPresetsAreExpressibleInFlags(t *testing.T) {
	body := weatherRequest(t)
	for _, preset := range catalog.PresetNames() {
		backend := catalog.Presets()[preset]
		// The quirk keys are capabilities of an endpoint, and the only
		// entries in the table that no flag restates. Everything else —
		// including the credential's variable and the upstream streaming
		// decision, which the protocol owns — must be reachable from the
		// command line unchanged.
		if backend.SchemaAsPrompt || backend.MaxOutputTokensAs != "" || backend.OmitDefaultMaxOutputTokens {
			continue
		}
		t.Run(preset, func(t *testing.T) {
			viaPreset, _ := translate(t, core.Options{Preset: preset}, body)
			viaFlags, _ := translate(t, core.Options{
				BackendProtocol: backend.Dialect,
				Endpoint:        backend.Endpoint,
				AuthEnv:         backend.KeyEnv,
			}, body)
			if !equalJSON(viaPreset, viaFlags) {
				t.Errorf("preset and flags disagree\npreset: %v\nflags:  %v", viaPreset, viaFlags)
			}
		})
	}
}

// TestRequiredCapTravelsWithTheProtocol covers what owning placement buys: the
// one wire format that will not accept a request without an output cap supplies
// its own, so selecting it by flags and selecting it by name cannot disagree. A
// service table that had to restate the cap would be a config file deciding a
// structural question, and an endpoint reached by flags would 400.
func TestRequiredCapTravelsWithTheProtocol(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	sent, _ := translate(t, core.Options{
		BackendProtocol: "anthropic-messages",
		Endpoint:        "https://api.anthropic.com/v1",
		AuthEnv:         "ANTHROPIC_API_KEY",
	}, body)
	if got := lookup(sent, "max_tokens"); got != "20000" {
		t.Errorf("max_tokens = %q, want %q from the protocol itself", got, "20000")
	}
}

func hasDropPath(drops []base.Drop, path string) bool {
	for _, drop := range drops {
		if drop.Path == path {
			return true
		}
	}
	return false
}

// lookup reads a dotted path out of a decoded body, rendering the leaf as a
// string so a table can state expectations plainly.
func lookup(body map[string]any, path string) string {
	var current any = body
	for _, segment := range strings.Split(path, ".") {
		switch typed := current.(type) {
		case map[string]any:
			value, present := typed[segment]
			if !present {
				return ""
			}
			current = value
		case []any:
			index, err := strconv.Atoi(segment)
			if err != nil || index < 0 || index >= len(typed) {
				return ""
			}
			current = typed[index]
		default:
			return ""
		}
	}
	if current == nil {
		return ""
	}
	encoded, err := json.Marshal(current)
	if err != nil {
		return ""
	}
	return strings.Trim(string(encoded), `"`)
}

func equalJSON(left, right map[string]any) bool {
	a, errA := json.Marshal(left)
	b, errB := json.Marshal(right)
	return errA == nil && errB == nil && string(a) == string(b)
}

// TestTranslationMatchesTheAcceptedFixture ties the offline suite to bytes a
// provider actually accepted. Each testdata/fixtures/<backend>/request-stream.json
// was produced by --no-execute --stream and then POSTed to the live endpoint;
// the stream.sse beside it is the 200 that came back. If an encoder changes,
// this fails and the fixture has to be recaptured — which re-proves acceptance
// rather than assuming it.
func TestTranslationMatchesTheAcceptedFixture(t *testing.T) {
	body := weatherRequest(t)
	models := map[string]string{
		"openai":    "gpt-5.4-mini",
		"anthropic": "claude-sonnet-5",
		"gemini":    "gemini-3.6-flash",
		"deepseek":  "deepseek-v4-flash",
	}
	for preset, model := range models {
		t.Run(preset, func(t *testing.T) {
			streamed := true
			sent, _ := translate(t, core.Options{Preset: preset, Model: model, Stream: &streamed}, body)

			path := filepath.Join("..", "..", "testdata", "fixtures", preset, "request-stream.json")
			recorded, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("fixture: %v", err)
			}
			var accepted map[string]any
			if err := json.Unmarshal(recorded, &accepted); err != nil {
				t.Fatalf("fixture: %v", err)
			}
			if !equalJSON(sent, accepted) {
				t.Errorf("translation drifted from the body the provider accepted\n got: %v\nwant: %v", sent, accepted)
			}
		})
	}
}
