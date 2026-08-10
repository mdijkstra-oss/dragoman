package core_test

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/mdijkstra-oss/dragoman/internal/base"
	"github.com/mdijkstra-oss/dragoman/internal/catalog"
	"github.com/mdijkstra-oss/dragoman/internal/core"
)

// The marker the caller sends. The keys are deliberately out of alphabetical
// order and one of them is a key no provider documents: the bytes are the
// caller's, so a dialect spelling the field OpenAI's way has to forward them
// exactly as they arrived rather than re-serialising a struct it understands.
const cacheMarker = `{"mode":"explicit","hint":"prefix","depth":3}`

// cachePlainMarker is the marker for the dialect that has to place its own,
// where every byte beyond the marker's presence is a reported drop and a second
// key would only make the assertion about two drops instead of one.
const cachePlainMarker = `{"mode":"explicit"}`

// The three dialects that spell the breakpoint the way the base does, paired
// with where the marked part lands in each one's body. openai-responses is the
// base, so the marker never moves; the other two flatten the item list into
// messages.
var cacheForwarding = []struct {
	preset string
	marker string
}{
	{preset: "openai", marker: "input.0.content.0.prompt_cache_breakpoint"},
	{preset: "openai-chat", marker: "messages.0.content.0.prompt_cache_breakpoint"},
	{preset: "deepseek", marker: "messages.0.content.0.prompt_cache_breakpoint"},
}

// TestCacheBreakpointCrossesTheOpenAIDialectsUnchanged is the spec's first
// offline row: a breakpoint on an input_text block survives byte-identical
// through all three OpenAI dialects, and the bytes are the same bytes in all
// three. Nothing about the marker is mapped, so anything less than equality
// means a struct was interposed between the caller and the provider.
func TestCacheBreakpointCrossesTheOpenAIDialectsUnchanged(t *testing.T) {
	body := cacheRequest(cacheParts(cacheMarker),
		`,"prompt_cache_key":"tenant-7","prompt_cache_options":{"ttl":"30m"}`)

	seen := map[string]string{}
	for _, forwarding := range cacheForwarding {
		t.Run(forwarding.preset, func(t *testing.T) {
			sent, drops := cacheSent(t, forwarding.preset, body)
			if got := cacheRaw(t, sent, forwarding.marker); got != cacheMarker {
				t.Errorf("marker = %s, want %s byte for byte", got, cacheMarker)
			}
			seen[forwarding.preset] = cacheRaw(t, sent, forwarding.marker)

			// Both caching fields are top-level and spelled the same at both
			// ends, so they are placed rather than mapped.
			if got := cacheRaw(t, sent, "prompt_cache_key"); got != `"tenant-7"` {
				t.Errorf("prompt_cache_key = %s, want %s", got, `"tenant-7"`)
			}
			if got := cacheRaw(t, sent, "prompt_cache_options"); got != `{"ttl":"30m"}` {
				t.Errorf("prompt_cache_options = %s, want the object that arrived", got)
			}
			// The typed field exists so the diff sees the marker consumed. A
			// drop here is the spurious kind: nothing was lost.
			if len(drops) != 0 {
				t.Errorf("drops = %+v, want none; every field was carried", drops)
			}
		})
	}

	t.Run("the base is a pass-through", func(t *testing.T) {
		sent, _ := cacheSent(t, "openai", body)
		if string(sent) != string(body) {
			t.Errorf("body was rewritten\n got: %s\nwant: %s", sent, body)
		}
	})

	t.Run("one set of marker bytes", func(t *testing.T) {
		for preset, marker := range seen {
			if marker != seen["openai"] {
				t.Errorf("%s sent %s, openai sent %s; the marker is identical across the three",
					preset, marker, seen["openai"])
			}
		}
	})
}

// TestCacheBreakpointBecomesEphemeralCacheControl is the row for the dialect
// that spells a breakpoint its own way: cache_control lands on the same block,
// and OpenAI's mode has no equivalent to land in, so it is reported at the path
// the caller can find rather than going missing.
func TestCacheBreakpointBecomesEphemeralCacheControl(t *testing.T) {
	body := cacheRequest(cacheParts(cachePlainMarker), "")
	sent, drops := cacheSent(t, "anthropic", body)

	const want = `{"type":"text","text":"part0","cache_control":{"type":"ephemeral"}}`
	if got := cacheRaw(t, sent, "messages.0.content.0"); got != want {
		t.Errorf("block = %s, want %s", got, want)
	}
	if strings.Contains(string(sent), "prompt_cache_breakpoint") {
		t.Errorf("OpenAI's spelling reached a dialect that does not have it: %s", sent)
	}

	const modePath = "input[0].content[0].prompt_cache_breakpoint.mode"
	if !hasDropPath(drops, modePath) {
		t.Errorf("drops = %+v, want one at %s", drops, modePath)
	}
}

// TestCacheTTLTravelsVerbatimAndAbsentStaysAbsent covers the two TTL rows at
// once, because they are one rule. The value is opaque: a 30m is placed as it
// arrived and refused by the provider, which is the point — converting it to a
// TTL the provider accepts would be dragoman choosing a price. And a request
// that named no TTL gets type alone, leaving the provider's own default.
func TestCacheTTLTravelsVerbatimAndAbsentStaysAbsent(t *testing.T) {
	cases := []struct {
		name string
		tail string
		want string
	}{
		{
			name: "no options at all",
			tail: "",
			want: `{"type":"ephemeral"}`,
		},
		{
			name: "options with no ttl",
			tail: `,"prompt_cache_options":{}`,
			want: `{"type":"ephemeral"}`,
		},
		{
			// A TTL this provider accepts, to show the placement is the same one.
			name: "a ttl the provider takes",
			tail: `,"prompt_cache_options":{"ttl":"1h"}`,
			want: `{"type":"ephemeral","ttl":"1h"}`,
		},
		{
			// OpenAI's only TTL, and one this provider refuses by name.
			name: "a ttl the provider refuses",
			tail: `,"prompt_cache_options":{"ttl":"30m"}`,
			want: `{"type":"ephemeral","ttl":"30m"}`,
		},
		{
			// Nothing validates this, so nothing can quietly round it to a
			// neighbouring bucket either.
			name: "a ttl nobody defines",
			tail: `,"prompt_cache_options":{"ttl":"7 fortnights"}`,
			want: `{"type":"ephemeral","ttl":"7 fortnights"}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			sent, _ := cacheSent(t, "anthropic", cacheRequest(cacheParts(cachePlainMarker), testCase.tail))
			if got := cacheRaw(t, sent, "messages.0.content.0.cache_control"); got != testCase.want {
				t.Errorf("cache_control = %s, want %s", got, testCase.want)
			}
			if testCase.want == `{"type":"ephemeral"}` && strings.Contains(string(sent), "ttl") {
				t.Errorf("a ttl key appeared where none was asked for: %s", sent)
			}
		})
	}
}

// TestFiveCacheBreakpointsAreForwardedUntrimmed covers the count row. The
// four-breakpoint limit is the provider's to enforce: a fifth is forwarded so
// the refusal names it, where trimming would silently spend the caller's fourth
// slot somewhere they did not choose.
func TestFiveCacheBreakpointsAreForwardedUntrimmed(t *testing.T) {
	all := cacheParts(cachePlainMarker, cachePlainMarker, cachePlainMarker, cachePlainMarker, cachePlainMarker)

	t.Run("anthropic", func(t *testing.T) {
		sent, _ := cacheSent(t, "anthropic", cacheRequest(all, ""))
		blocks := cacheElements(t, sent, "messages.0.content")
		if len(blocks) != 5 {
			t.Fatalf("blocks = %d, want 5: a marker sits on a block, so five markers are five blocks", len(blocks))
		}
		for index, block := range blocks {
			want := `{"type":"text","text":"part` + strconv.Itoa(index) +
				`","cache_control":{"type":"ephemeral"}}`
			if block != want {
				t.Errorf("block %d = %s, want %s", index, block, want)
			}
		}
		if got := strings.Count(string(sent), "cache_control"); got != 5 {
			t.Errorf("cache_control appears %d times, want 5", got)
		}
	})

	// Marking the third of five parts means five blocks with the marker on the
	// third, and dragoman never adds one to the other four.
	t.Run("anthropic marks only what was marked", func(t *testing.T) {
		body := cacheRequest(cacheParts("", "", cachePlainMarker, "", ""), "")
		sent, _ := cacheSent(t, "anthropic", body)
		blocks := cacheElements(t, sent, "messages.0.content")
		if len(blocks) != 5 {
			t.Fatalf("blocks = %d, want 5", len(blocks))
		}
		for index, block := range blocks {
			marked := strings.Contains(block, "cache_control")
			if marked != (index == 2) {
				t.Errorf("block %d = %s; only the marked part carries a cache_control", index, block)
			}
		}
	})

	for _, forwarding := range cacheForwarding {
		t.Run(forwarding.preset, func(t *testing.T) {
			sent, _ := cacheSent(t, forwarding.preset, cacheRequest(all, ""))
			if got := strings.Count(string(sent), "prompt_cache_breakpoint"); got != 5 {
				t.Errorf("prompt_cache_breakpoint appears %d times, want 5: %s", got, sent)
			}
		})
	}
}

// TestUnrecognisedCacheModeIsForwardedNotInspected covers the row that keeps
// the value opaque. A mode is OpenAI's set, not dragoman's, so a string nothing
// here has heard of is carried where the field exists and reported where it does
// not — never validated, and never a panic.
func TestUnrecognisedCacheModeIsForwardedNotInspected(t *testing.T) {
	const marker = `{"mode":"eventually-if-it-helps"}`
	body := cacheRequest(cacheParts(marker), "")

	for _, forwarding := range cacheForwarding {
		t.Run(forwarding.preset, func(t *testing.T) {
			sent, drops := cacheSent(t, forwarding.preset, body)
			if got := cacheRaw(t, sent, forwarding.marker); got != marker {
				t.Errorf("marker = %s, want %s: the mode is the caller's value", got, marker)
			}
			if len(drops) != 0 {
				t.Errorf("drops = %+v, want none; the marker was carried whole", drops)
			}
		})
	}

	t.Run("anthropic reports it and still marks the block", func(t *testing.T) {
		sent, drops := cacheSent(t, "anthropic", body)
		if got := cacheRaw(t, sent, "messages.0.content.0.cache_control"); got != `{"type":"ephemeral"}` {
			t.Errorf("cache_control = %s, want the marker placed regardless of its mode", got)
		}
		if !hasDropPath(drops, "input[0].content[0].prompt_cache_breakpoint.mode") {
			t.Errorf("drops = %+v, want the mode named at its path", drops)
		}
		if strings.Contains(string(sent), "eventually-if-it-helps") {
			t.Errorf("a mode this dialect has no field for was sent anyway: %s", sent)
		}
	})

	t.Run("gemini has no field at all", func(t *testing.T) {
		sent, drops := cacheSent(t, "gemini", body)
		for _, spelling := range []string{"prompt_cache_breakpoint", "cache_control", "eventually-if-it-helps"} {
			if strings.Contains(string(sent), spelling) {
				t.Errorf("%q reached a dialect with no breakpoint: %s", spelling, sent)
			}
		}
		if !hasDropPath(drops, "input[0].content[0].prompt_cache_breakpoint") {
			t.Errorf("drops = %+v, want the whole marker named at its path", drops)
		}
	})
}

// TestCacheAbsentLeavesTheBodyAsItWas is the regression the feature is judged
// on: keeping content parts so a marker can ride on one must not change the
// shape of a body that carries no marker. A part array where a bare string used
// to go is a new request for every caller who never asked for caching.
func TestCacheAbsentLeavesTheBodyAsItWas(t *testing.T) {
	body := cacheRequest(cacheParts(""), "")

	t.Run("openai", func(t *testing.T) {
		sent, drops := cacheSent(t, "openai", body)
		if string(sent) != string(body) {
			t.Errorf("body was rewritten\n got: %s\nwant: %s", sent, body)
		}
		if len(drops) != 0 {
			t.Errorf("drops = %+v, want none", drops)
		}
	})

	for _, preset := range []string{"openai-chat", "deepseek"} {
		t.Run(preset, func(t *testing.T) {
			sent, _ := cacheSent(t, preset, body)
			if got := cacheRaw(t, sent, "messages.0.content"); got != `"part0"` {
				t.Errorf("content = %s, want the bare string form it has always sent", got)
			}
		})
	}

	t.Run("anthropic", func(t *testing.T) {
		sent, _ := cacheSent(t, "anthropic", body)
		const want = `{"type":"text","text":"part0"}`
		if got := cacheRaw(t, sent, "messages.0.content.0"); got != want {
			t.Errorf("block = %s, want %s", got, want)
		}
	})

	// Two parts with no marker still collapse into one joined block, gap and
	// all, because nothing forced them to be spelled out.
	t.Run("unmarked parts stay joined", func(t *testing.T) {
		twoParts := cacheRequest(cacheParts("", ""), "")
		sent, _ := cacheSent(t, "openai-chat", twoParts)
		if got := cacheRaw(t, sent, "messages.0.content"); got != `"part0part1"` {
			t.Errorf("content = %s, want the parts folded into one string", got)
		}
		sent, _ = cacheSent(t, "anthropic", twoParts)
		blocks := cacheElements(t, sent, "messages.0.content")
		if len(blocks) != 1 || blocks[0] != `{"type":"text","text":"part0part1"}` {
			t.Errorf("blocks = %v, want the one joined block", blocks)
		}
	})
}

// TestCacheMarkerSurvivesAFlagRewrite covers the pass-through's one hazard: a
// flag that changes a field forces the base's encoder to rewrite the body it
// would otherwise have forwarded whole. The marker is not a field any flag
// touches, so it has to come out the other side unchanged.
func TestCacheMarkerSurvivesAFlagRewrite(t *testing.T) {
	body := cacheRequest(cacheParts(cacheMarker), `,"prompt_cache_options":{"ttl":"30m"}`)
	streamed := true
	result, err := core.Run(context.Background(), catalog.New(),
		core.Options{Preset: "openai", NoExecute: true, Model: "gpt-5.6-luna",
			Effort: "low", MaxTokens: capOf(64), Stream: &streamed}, body, nil, nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if got := cacheRaw(t, result.Sent, "input.0.content.0.prompt_cache_breakpoint"); got != cacheMarker {
		t.Errorf("marker = %s, want %s; a rewritten body still carries it", got, cacheMarker)
	}
	if got := cacheRaw(t, result.Sent, "prompt_cache_options"); got != `{"ttl":"30m"}` {
		t.Errorf("prompt_cache_options = %s, want the object that arrived", got)
	}
	if got := cacheRaw(t, result.Sent, "model"); got != `"gpt-5.6-luna"` {
		t.Errorf("model = %s, want the flag applied; otherwise nothing was rewritten and the case is inert", got)
	}
}

// TestCacheMarkerInsideACollapsedAssistantRun covers the one place where
// keeping parts changes a body's shape for a reason unrelated to the marker: a
// run of assistant items is folded into one message by joining their text, and
// once a breakpoint forces the parts to be spelled out the blank line between
// two of them is prompt text with nowhere to live. The concatenated parts have
// to be the same string the joined form sent.
func TestCacheMarkerInsideACollapsedAssistantRun(t *testing.T) {
	run := func(marker string) []byte {
		second := `{"type":"output_text","text":"Second."`
		if marker != "" {
			second += `,"prompt_cache_breakpoint":` + marker
		}
		return []byte(`{"model":"m","input":[` + oneUserTurn + `,` +
			`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"First."}]},` +
			`{"type":"message","role":"assistant","content":[` + second + `}]}]}`)
	}

	t.Run("openai-chat", func(t *testing.T) {
		joinedSent, _ := cacheSent(t, "openai-chat", run(""))
		var joined string
		if err := json.Unmarshal([]byte(cacheRaw(t, joinedSent, "messages.1.content")), &joined); err != nil {
			t.Fatalf("the unmarked run is not the string form: %v", err)
		}

		markedSent, _ := cacheSent(t, "openai-chat", run(cachePlainMarker))
		var parts []struct {
			Text   string          `json:"text"`
			Marker json.RawMessage `json:"prompt_cache_breakpoint"`
		}
		if err := json.Unmarshal([]byte(cacheRaw(t, markedSent, "messages.1.content")), &parts); err != nil {
			t.Fatalf("the marked run is not the parts form: %v", err)
		}
		if len(parts) != 2 {
			t.Fatalf("parts = %d, want 2: neither item may be dropped to make room for a marker", len(parts))
		}
		if spelled := parts[0].Text + parts[1].Text; spelled != joined {
			t.Errorf("parts concatenate to %q, want %q: the gap between two items is text the model reads",
				spelled, joined)
		}
		if len(parts[0].Marker) != 0 || string(parts[1].Marker) != cachePlainMarker {
			t.Errorf("marker landed on %v, want it on the second part alone", parts)
		}
	})

	t.Run("anthropic", func(t *testing.T) {
		sent, _ := cacheSent(t, "anthropic", run(cachePlainMarker))
		blocks := cacheElements(t, sent, "messages.1.content")
		want := []string{
			`{"type":"text","text":"First."}`,
			`{"type":"text","text":"Second.","cache_control":{"type":"ephemeral"}}`,
		}
		if len(blocks) != len(want) {
			t.Fatalf("blocks = %v, want %v", blocks, want)
		}
		for index, block := range blocks {
			if block != want[index] {
				t.Errorf("block %d = %s, want %s", index, block, want[index])
			}
		}
	})
}

// TestCacheMarkerOnSystemTextIsReportedNotSilent covers the placement that
// cannot be mapped rather than the ones that can. One dialect keeps instructions
// as a single string, so a marker on text folded in there has no block left to
// sit on — and a marker that vanishes is a caller paying full price believing
// they asked not to.
func TestCacheMarkerOnSystemTextIsReportedNotSilent(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"message","role":"system","content":` +
		`[{"type":"input_text","text":"be terse","prompt_cache_breakpoint":` + cachePlainMarker + `}]},` +
		oneUserTurn + `]}`)

	sent, drops := cacheSent(t, "anthropic", body)
	if got := cacheRaw(t, sent, "system"); got != `"be terse"` {
		t.Errorf("system = %s, want the instructions folded in as before", got)
	}
	if !hasDropPath(drops, "input[0].content[0].prompt_cache_breakpoint") {
		t.Errorf("drops = %+v, want the unplaceable marker named at its path", drops)
	}

	// The dialects that keep a system turn as a message keep the marker on it.
	for _, preset := range []string{"openai-chat", "deepseek"} {
		t.Run(preset, func(t *testing.T) {
			sent, drops := cacheSent(t, preset, body)
			if got := cacheRaw(t, sent, "messages.0.content.0.prompt_cache_breakpoint"); got != cachePlainMarker {
				t.Errorf("marker = %s, want %s on the system message", got, cachePlainMarker)
			}
			if len(drops) != 0 {
				t.Errorf("drops = %+v, want none", drops)
			}
		})
	}
}

// cacheSent runs the whole pipeline minus the network and returns the bytes that
// would have gone out, unparsed: the claim under test is about bytes, and
// decoding them first would hide a re-serialisation.
func cacheSent(t *testing.T, preset string, body []byte) ([]byte, []base.Drop) {
	t.Helper()
	result, err := core.Run(context.Background(), catalog.New(),
		core.Options{Preset: preset, NoExecute: true}, body, nil, nil)
	if err != nil {
		t.Fatalf("translate for %s: %v", preset, err)
	}
	return result.Sent, result.Drops
}

// cacheRequest builds a Responses request carrying one user message whose
// content is the parts given, plus whatever top-level fields the case names.
// The cap is stated so the base dialect has nothing to fill in and forwards
// the bytes untouched.
func cacheRequest(parts, tail string) []byte {
	return []byte(`{"model":"m","input":[{"type":"message","role":"user","content":` +
		parts + `}],"max_output_tokens":20000` + tail + `}`)
}

// cacheParts builds a content array, one part per marker; an empty marker
// leaves that part unmarked.
func cacheParts(markers ...string) string {
	built := make([]string, 0, len(markers))
	for index, marker := range markers {
		part := `{"type":"input_text","text":"part` + strconv.Itoa(index) + `"`
		if marker != "" {
			part += `,"prompt_cache_breakpoint":` + marker
		}
		built = append(built, part+`}`)
	}
	return `[` + strings.Join(built, ",") + `]`
}

// cacheRaw reads one value out of a body as the bytes it occupies there, so a
// test can state key order and the absence of a key rather than only a decoded
// value. A segment that is a number indexes an array.
func cacheRaw(t *testing.T, body []byte, path string) string {
	t.Helper()
	current := json.RawMessage(body)
	for _, segment := range strings.Split(path, ".") {
		if index, err := strconv.Atoi(segment); err == nil {
			elements := cacheList(t, current)
			if index >= len(elements) {
				t.Fatalf("%s: index %d of %d in %s", path, index, len(elements), current)
			}
			current = elements[index]
			continue
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(current, &object); err != nil {
			t.Fatalf("%s: %v in %s", path, err, current)
		}
		value, present := object[segment]
		if !present {
			return ""
		}
		current = value
	}
	return string(current)
}

// cacheElements reads a list as the bytes each element occupies.
func cacheElements(t *testing.T, body []byte, path string) []string {
	t.Helper()
	elements := cacheList(t, json.RawMessage(cacheRaw(t, body, path)))
	rendered := make([]string, 0, len(elements))
	for _, element := range elements {
		rendered = append(rendered, string(element))
	}
	return rendered
}

func cacheList(t *testing.T, raw json.RawMessage) []json.RawMessage {
	t.Helper()
	var elements []json.RawMessage
	if err := json.Unmarshal(raw, &elements); err != nil {
		t.Fatalf("not a list: %v in %s", err, raw)
	}
	return elements
}
