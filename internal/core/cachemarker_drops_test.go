package core_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/matthijn/dragoman/internal/base"
	"github.com/matthijn/dragoman/internal/catalog"
	"github.com/matthijn/dragoman/internal/core"
)

// The five presets are the five wire formats, one each: openai is
// openai-responses, openai-chat is openai-chat-completions, deepseek is
// openai-completions, and the other two name themselves. claude-cli is not a
// sixth — it speaks anthropic-messages.
var cacheDropDialects = []string{"anthropic", "deepseek", "gemini", "openai", "openai-chat"}

// cacheDropBody is one user turn of two parts with the marker on the second,
// plus both top-level caching fields. Every path this file asserts on is
// reachable from it: a breakpoint whose mode has nowhere to go, options whose
// mode has nowhere to go, and a key with no equivalent at all.
const cacheDropBody = `{"model":"m","prompt_cache_key":"tenant-7",` +
	`"prompt_cache_options":{"ttl":"30m","mode":"explicit"},` +
	`"input":[{"type":"message","role":"user","content":[` +
	`{"type":"input_text","text":"the long stable prefix"},` +
	`{"type":"input_text","text":"the tail","prompt_cache_breakpoint":{"mode":"explicit"}}]}]}`

// TestCacheBreakpointAgainstGeminiIsUnsupported covers the row for the dialect
// with no field for a breakpoint in any spelling. Both of its caching routes are
// stateful, so this is permanent rather than pending — and the caller is told at
// the path they wrote, because paying full price on a request they marked is the
// silence the drop diff exists to prevent.
func TestCacheBreakpointAgainstGeminiIsUnsupported(t *testing.T) {
	sent, drops := cacheDropSent(t, "gemini", cacheDropBody)

	// The path names the block, not the field: input[i].content[j] is the only
	// depth a marker is reachable at, and the second part is the marked one.
	const marker = "input[0].content[1].prompt_cache_breakpoint"
	if reason := cacheDropReasons(drops)[marker]; reason != base.DropUnsupported {
		t.Errorf("%s = %q, want %q; drops = %+v", marker, reason, base.DropUnsupported, drops)
	}
	if !mentions(drops, "no cache breakpoint") {
		t.Errorf("the drop does not say why it is permanent: %+v", drops)
	}
	// The unmarked first part is not a drop. A marker dragoman reports where the
	// caller placed none is as misleading as one it fails to report.
	if hasDropPath(drops, "input[0].content[0].prompt_cache_breakpoint") {
		t.Errorf("an unmarked part was reported as dropped: %+v", drops)
	}
	for _, spelling := range []string{"prompt_cache_breakpoint", "cache_control", "tenant-7", "30m"} {
		if strings.Contains(sent, spelling) {
			t.Errorf("%q reached a dialect that has no field for it: %s", spelling, sent)
		}
	}

	// Both top-level fields go the same way, whole: there is nothing here that
	// half of either one could land in.
	for _, path := range []string{"prompt_cache_key", "prompt_cache_options"} {
		if reason := cacheDropReasons(drops)[path]; reason != base.DropUnsupported {
			t.Errorf("%s = %q, want %q; drops = %+v", path, reason, base.DropUnsupported, drops)
		}
	}

	t.Run("one drop per marker", func(t *testing.T) {
		_, drops := cacheDropSent(t, "gemini", cacheDropMarked(5))
		for index := range 5 {
			path := "input[0].content[" + strconv.Itoa(index) + "].prompt_cache_breakpoint"
			if !hasDropPath(drops, path) {
				t.Errorf("%s went unreported; drops = %+v", path, drops)
			}
		}
	})
}

// TestCacheOptionsModeAgainstAnthropicIsDropped covers the options row: a
// cache_control has a type and a TTL and nothing that means OpenAI's mode. The
// TTL is not a drop — it is placed verbatim on the block — so exactly the part
// that could not travel is named, and the whole object is not condemned with it.
func TestCacheOptionsModeAgainstAnthropicIsDropped(t *testing.T) {
	sent, drops := cacheDropSent(t, "anthropic", cacheDropBody)
	reasons := cacheDropReasons(drops)

	const mode = "prompt_cache_options.mode"
	if reasons[mode] != base.DropUnsupported {
		t.Errorf("%s = %q, want %q; drops = %+v", mode, reasons[mode], base.DropUnsupported, drops)
	}
	if _, reported := reasons["prompt_cache_options"]; reported {
		t.Errorf("the whole object was reported as dropped, but its ttl travelled: %+v", drops)
	}
	if _, reported := reasons["prompt_cache_options.ttl"]; reported {
		t.Errorf("the ttl was reported as dropped, but it is placed on the block: %+v", drops)
	}
	if !strings.Contains(sent, `"cache_control":{"type":"ephemeral","ttl":"30m"}`) {
		t.Errorf("the ttl did not reach the block it fans out onto: %s", sent)
	}
	if strings.Contains(sent, "explicit") {
		t.Errorf("a mode with no field to land in was sent anyway: %s", sent)
	}

	// The breakpoint's own mode is the second half of the same rule, at its own
	// path. Both are the caller's bytes, and neither is guessed at.
	const marker = "input[0].content[1].prompt_cache_breakpoint.mode"
	if reasons[marker] != base.DropUnsupported {
		t.Errorf("%s = %q, want %q; drops = %+v", marker, reasons[marker], base.DropUnsupported, drops)
	}
}

// TestCacheKeyAgainstAnthropicIsDropped covers the key row. This dialect has no
// cache key of any kind — the prefix is the key — so the field is unsupported
// rather than a mapping nobody has written yet, and it never reaches the wire
// under any name.
func TestCacheKeyAgainstAnthropicIsDropped(t *testing.T) {
	sent, drops := cacheDropSent(t, "anthropic", cacheDropBody)

	if reason := cacheDropReasons(drops)["prompt_cache_key"]; reason != base.DropUnsupported {
		t.Errorf("prompt_cache_key = %q, want %q; drops = %+v", reason, base.DropUnsupported, drops)
	}
	for _, spelling := range []string{"prompt_cache_key", "tenant-7", "cache_key"} {
		if strings.Contains(sent, spelling) {
			t.Errorf("%q reached a dialect with no cache key: %s", spelling, sent)
		}
	}
}

// TestStrictNamesEveryDroppedCachePath is the flag for the caller who wants to
// hear about every difference: exit 2, nothing sent, and every path the default
// run reported on stderr named in the failure. A strict run that reported three
// of four drops would be worse than no flag, because it reads as a clean bill.
func TestStrictNamesEveryDroppedCachePath(t *testing.T) {
	for _, preset := range []string{"anthropic", "gemini"} {
		t.Run(preset, func(t *testing.T) {
			_, drops := cacheDropSent(t, preset, cacheDropBody)
			if len(drops) == 0 {
				t.Fatal("nothing was dropped, so strict has nothing to promote")
			}

			_, err := core.Run(context.Background(), catalog.New(),
				core.Options{Preset: preset, NoExecute: true, Strict: true}, []byte(cacheDropBody), nil, nil)
			if err == nil {
				t.Fatal("strict accepted a request whose caching fields were dropped")
			}
			if core.CodeOf(err) != core.ExitUsage {
				t.Errorf("exit = %d, want %d: dragoman could not run", core.CodeOf(err), core.ExitUsage)
			}
			for _, drop := range drops {
				if !strings.Contains(err.Error(), drop.Path) {
					t.Errorf("strict does not name %s: %v", drop.Path, err)
				}
			}
		})
	}
}

// TestCacheFieldsAreNoDropWhereTheyAreCarried is the base.Diff behaviour the
// typed fields exist for. The three dialects that spell all three fields the way
// the base does place the caller's bytes and lose nothing, so a drop reported
// against them is spurious: it would teach a caller to ignore a warning that
// elsewhere means their prompt is being paid for at full price.
func TestCacheFieldsAreNoDropWhereTheyAreCarried(t *testing.T) {
	for _, preset := range []string{"openai", "openai-chat", "deepseek"} {
		t.Run(preset, func(t *testing.T) {
			sent, drops := cacheDropSent(t, preset, cacheDropBody)
			if len(drops) != 0 {
				t.Errorf("drops = %+v, want none; every field was carried", drops)
			}
			for _, spelling := range []string{`"prompt_cache_breakpoint":{"mode":"explicit"}`,
				`"prompt_cache_key":"tenant-7"`, `"prompt_cache_options":{"ttl":"30m","mode":"explicit"}`} {
				if !strings.Contains(sent, spelling) {
					t.Errorf("%s is missing from the body: %s", spelling, sent)
				}
			}
		})
	}

	// The dialect that places its own marker still consumed the field, so what is
	// reported is the mode inside it and never the marker itself.
	t.Run("anthropic reports the mode and not the marker", func(t *testing.T) {
		_, drops := cacheDropSent(t, "anthropic", cacheDropBody)
		if hasDropPath(drops, "input[0].content[1].prompt_cache_breakpoint") {
			t.Errorf("the marker was reported as lost, but it became a cache_control: %+v", drops)
		}
	})
}

// TestNoCacheMarkerIsByteIdenticalToThePreFeatureWire is the regression this
// feature is judged on, and the reason the want strings below are bytes rather
// than a decoded shape. Each one was produced by the commit before breakpoints
// existed, at --no-execute, buffered and streamed, over the bodies whose
// handling the change touched: a bare string input, multiple content parts, a
// collapsed assistant run whose text is joined with a gap, empty parts, a system
// turn, tools with a call and a result, and the fixture the live matrix sends. A
// caller who never asked for caching must not get a new request shape — a parts
// array where a bare string used to go is a different body for every one of
// them.
func TestNoCacheMarkerIsByteIdenticalToThePreFeatureWire(t *testing.T) {
	for _, testCase := range noMarkerGolden {
		t.Run(testCase.name, func(t *testing.T) {
			for _, preset := range cacheDropDialects {
				for _, streamed := range []bool{false, true} {
					mode := "buffered"
					if streamed {
						mode = "streamed"
					}
					key := preset + "/" + mode
					t.Run(key, func(t *testing.T) {
						want, recorded := testCase.want[key]
						if !recorded {
							t.Fatalf("no pre-feature bytes recorded for %s", key)
						}
						result, err := core.Run(context.Background(), catalog.New(),
							core.Options{Preset: preset, NoExecute: true, Stream: &streamed},
							[]byte(testCase.body), nil, nil)
						if err != nil {
							t.Fatalf("translate: %v", err)
						}
						if string(result.Sent) != want {
							t.Errorf("the wire body changed for a request with no marker\n got: %s\nwant: %s",
								result.Sent, want)
						}
						if mentions(result.Drops, "prompt_cache") || mentions(result.Drops, "cache_control") {
							t.Errorf("a request with no caching field was told about one: %+v", result.Drops)
						}
					})
				}
			}
		})
	}
}

// TestCacheMarkerWrittenAsNullIsNotABreakpoint covers the shape a client that
// serialises its unset fields produces. Everywhere else in the base a JSON null
// means the key was not sent — every pointer field decodes it to absent, and the
// drop diff treats it as carrying nothing — so a null here cannot be the one
// place it means the opposite. It has to be: a breakpoint is a spending
// decision, dragoman never makes one, and against the dialect with a four-block
// limit an invented marker per part is a 400 on a request that asked for nothing.
func TestCacheMarkerWrittenAsNullIsNotABreakpoint(t *testing.T) {
	const body = `{"model":"m","input":[{"type":"message","role":"user","content":[` +
		`{"type":"input_text","text":"part0","prompt_cache_breakpoint":null},` +
		`{"type":"input_text","text":"part1","prompt_cache_breakpoint":null}]}]}`

	t.Run("anthropic marks nothing", func(t *testing.T) {
		sent, drops := cacheDropSent(t, "anthropic", body)
		if strings.Contains(sent, "cache_control") {
			t.Errorf("dragoman added a breakpoint the caller did not send: %s", sent)
		}
		if len(drops) != 0 {
			t.Errorf("drops = %+v, want none; nothing was sent to lose", drops)
		}
	})

	t.Run("gemini reports nothing", func(t *testing.T) {
		_, drops := cacheDropSent(t, "gemini", body)
		if mentions(drops, "prompt_cache") {
			t.Errorf("a marker nobody sent was reported as dropped: %+v", drops)
		}
	})

	for _, preset := range []string{"openai-chat", "deepseek"} {
		t.Run(preset+" keeps the string form", func(t *testing.T) {
			sent, _ := cacheDropSent(t, preset, body)
			if !strings.Contains(sent, `"content":"part0part1"`) {
				t.Errorf("content was spelled out as parts for a body with no marker: %s", sent)
			}
		})
	}
}

// cacheDropSent runs the whole pipeline minus the network and returns the bytes
// that would have gone out as a string, unparsed, plus what was reported as
// dropped. The claims here are about paths and bytes, and decoding first would
// hide both a re-serialisation and a key that is present as null.
func cacheDropSent(t *testing.T, preset, body string) (string, []base.Drop) {
	t.Helper()
	result, err := core.Run(context.Background(), catalog.New(),
		core.Options{Preset: preset, NoExecute: true}, []byte(body), nil, nil)
	if err != nil {
		t.Fatalf("translate for %s: %v", preset, err)
	}
	return string(result.Sent), result.Drops
}

// cacheDropReasons indexes drops by path, so a table can state the reason a path
// was reported with and not merely that it appeared.
func cacheDropReasons(drops []base.Drop) map[string]base.DropReason {
	reasons := map[string]base.DropReason{}
	for _, drop := range drops {
		reasons[drop.Path] = drop.Reason
	}
	return reasons
}

// cacheDropMarked builds one user turn whose every part carries a marker.
func cacheDropMarked(parts int) string {
	built := make([]string, 0, parts)
	for index := range parts {
		built = append(built, `{"type":"input_text","text":"part`+strconv.Itoa(index)+
			`","prompt_cache_breakpoint":{"mode":"explicit"}}`)
	}
	return `{"model":"m","input":[{"type":"message","role":"user","content":[` +
		strings.Join(built, ",") + `]}]}`
}

// noMarkerCase is a request that mentions no caching field, paired with the
// bytes each dialect sent for it before this feature existed, keyed by
// preset/mode.
type noMarkerCase struct {
	name string
	body string
	want map[string]string
}

var noMarkerGolden = []noMarkerCase{
	{
		name: "collapsed assistant run",
		body: `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"First."}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Second."}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"more"}]}]}`,
		want: map[string]string{
			"anthropic/buffered":   `{"model":"m","max_tokens":20000,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"assistant","content":[{"type":"text","text":"First."},{"type":"text","text":"Second."}]},{"role":"user","content":[{"type":"text","text":"more"}]}],"stream":true}`,
			"anthropic/streamed":   `{"model":"m","max_tokens":20000,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"assistant","content":[{"type":"text","text":"First."},{"type":"text","text":"Second."}]},{"role":"user","content":[{"type":"text","text":"more"}]}],"stream":true}`,
			"deepseek/buffered":    `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"First.\n\nSecond."},{"role":"user","content":"more"}],"model":"m","stream":false}`,
			"deepseek/streamed":    `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"First.\n\nSecond."},{"role":"user","content":"more"}],"model":"m","stream":true}`,
			"gemini/buffered":      `{"model":"m","input":[{"type":"user_input","content":[{"type":"text","text":"hi"}]},{"type":"model_output","content":[{"type":"text","text":"First."}]},{"type":"model_output","content":[{"type":"text","text":"Second."}]},{"type":"user_input","content":[{"type":"text","text":"more"}]}],"stream":false}`,
			"gemini/streamed":      `{"model":"m","input":[{"type":"user_input","content":[{"type":"text","text":"hi"}]},{"type":"model_output","content":[{"type":"text","text":"First."}]},{"type":"model_output","content":[{"type":"text","text":"Second."}]},{"type":"user_input","content":[{"type":"text","text":"more"}]}],"stream":true}`,
			"openai/buffered":      `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"First."}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Second."}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"more"}]}]}`,
			"openai/streamed":      `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"First."}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Second."}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"more"}]}],"model":"m","stream":true}`,
			"openai-chat/buffered": `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"First.\n\nSecond."},{"role":"user","content":"more"}],"model":"m","stream":false}`,
			"openai-chat/streamed": `{"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"First.\n\nSecond."},{"role":"user","content":"more"}],"model":"m","stream":true,"stream_options":{"include_usage":true}}`,
		},
	},
	{
		name: "empty content",
		body: `{"model":"m","input":[{"type":"message","role":"user","content":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":""}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
		want: map[string]string{
			"anthropic/buffered":   `{"model":"m","max_tokens":20000,"messages":[{"role":"user","content":[{"type":"text","text":""}]},{"role":"assistant","content":[{"type":"text","text":""}]},{"role":"user","content":[{"type":"text","text":"hi"}]}],"stream":true}`,
			"anthropic/streamed":   `{"model":"m","max_tokens":20000,"messages":[{"role":"user","content":[{"type":"text","text":""}]},{"role":"assistant","content":[{"type":"text","text":""}]},{"role":"user","content":[{"type":"text","text":"hi"}]}],"stream":true}`,
			"deepseek/buffered":    `{"messages":[{"role":"user"},{"role":"assistant"},{"role":"user","content":"hi"}],"model":"m","stream":false}`,
			"deepseek/streamed":    `{"messages":[{"role":"user"},{"role":"assistant"},{"role":"user","content":"hi"}],"model":"m","stream":true}`,
			"gemini/buffered":      `{"model":"m","input":[{"type":"user_input","content":[{"type":"text","text":""}]},{"type":"model_output","content":[{"type":"text","text":""}]},{"type":"user_input","content":[{"type":"text","text":"hi"}]}],"stream":false}`,
			"gemini/streamed":      `{"model":"m","input":[{"type":"user_input","content":[{"type":"text","text":""}]},{"type":"model_output","content":[{"type":"text","text":""}]},{"type":"user_input","content":[{"type":"text","text":"hi"}]}],"stream":true}`,
			"openai/buffered":      `{"model":"m","input":[{"type":"message","role":"user","content":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":""}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
			"openai/streamed":      `{"input":[{"type":"message","role":"user","content":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":""}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"model":"m","stream":true}`,
			"openai-chat/buffered": `{"messages":[{"role":"user"},{"role":"assistant"},{"role":"user","content":"hi"}],"model":"m","stream":false}`,
			"openai-chat/streamed": `{"messages":[{"role":"user"},{"role":"assistant"},{"role":"user","content":"hi"}],"model":"m","stream":true,"stream_options":{"include_usage":true}}`,
		},
	},
	{
		name: "multi part user",
		body: `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"first part "},{"type":"input_text","text":"second part"}]}]}`,
		want: map[string]string{
			"anthropic/buffered":   `{"model":"m","max_tokens":20000,"messages":[{"role":"user","content":[{"type":"text","text":"first part second part"}]}],"stream":true}`,
			"anthropic/streamed":   `{"model":"m","max_tokens":20000,"messages":[{"role":"user","content":[{"type":"text","text":"first part second part"}]}],"stream":true}`,
			"deepseek/buffered":    `{"messages":[{"role":"user","content":"first part second part"}],"model":"m","stream":false}`,
			"deepseek/streamed":    `{"messages":[{"role":"user","content":"first part second part"}],"model":"m","stream":true}`,
			"gemini/buffered":      `{"model":"m","input":[{"type":"user_input","content":[{"type":"text","text":"first part second part"}]}],"stream":false}`,
			"gemini/streamed":      `{"model":"m","input":[{"type":"user_input","content":[{"type":"text","text":"first part second part"}]}],"stream":true}`,
			"openai/buffered":      `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"first part "},{"type":"input_text","text":"second part"}]}]}`,
			"openai/streamed":      `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"first part "},{"type":"input_text","text":"second part"}]}],"model":"m","stream":true}`,
			"openai-chat/buffered": `{"messages":[{"role":"user","content":"first part second part"}],"model":"m","stream":false}`,
			"openai-chat/streamed": `{"messages":[{"role":"user","content":"first part second part"}],"model":"m","stream":true,"stream_options":{"include_usage":true}}`,
		},
	},
	{
		name: "one user turn",
		body: `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
		want: map[string]string{
			"anthropic/buffered":   `{"model":"m","max_tokens":20000,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"stream":true}`,
			"anthropic/streamed":   `{"model":"m","max_tokens":20000,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"stream":true}`,
			"deepseek/buffered":    `{"messages":[{"role":"user","content":"hi"}],"model":"m","stream":false}`,
			"deepseek/streamed":    `{"messages":[{"role":"user","content":"hi"}],"model":"m","stream":true}`,
			"gemini/buffered":      `{"model":"m","input":[{"type":"user_input","content":[{"type":"text","text":"hi"}]}],"stream":false}`,
			"gemini/streamed":      `{"model":"m","input":[{"type":"user_input","content":[{"type":"text","text":"hi"}]}],"stream":true}`,
			"openai/buffered":      `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
			"openai/streamed":      `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"model":"m","stream":true}`,
			"openai-chat/buffered": `{"messages":[{"role":"user","content":"hi"}],"model":"m","stream":false}`,
			"openai-chat/streamed": `{"messages":[{"role":"user","content":"hi"}],"model":"m","stream":true,"stream_options":{"include_usage":true}}`,
		},
	},
	{
		name: "string input",
		body: `{"model":"m","input":"Say hi."}`,
		want: map[string]string{
			"anthropic/buffered":   `{"model":"m","max_tokens":20000,"messages":[{"role":"user","content":[{"type":"text","text":"Say hi."}]}],"stream":true}`,
			"anthropic/streamed":   `{"model":"m","max_tokens":20000,"messages":[{"role":"user","content":[{"type":"text","text":"Say hi."}]}],"stream":true}`,
			"deepseek/buffered":    `{"messages":[{"role":"user","content":"Say hi."}],"model":"m","stream":false}`,
			"deepseek/streamed":    `{"messages":[{"role":"user","content":"Say hi."}],"model":"m","stream":true}`,
			"gemini/buffered":      `{"model":"m","input":[{"type":"user_input","content":[{"type":"text","text":"Say hi."}]}],"stream":false}`,
			"gemini/streamed":      `{"model":"m","input":[{"type":"user_input","content":[{"type":"text","text":"Say hi."}]}],"stream":true}`,
			"openai/buffered":      `{"model":"m","input":"Say hi."}`,
			"openai/streamed":      `{"input":"Say hi.","model":"m","stream":true}`,
			"openai-chat/buffered": `{"messages":[{"role":"user","content":"Say hi."}],"model":"m","stream":false}`,
			"openai-chat/streamed": `{"messages":[{"role":"user","content":"Say hi."}],"model":"m","stream":true,"stream_options":{"include_usage":true}}`,
		},
	},
	{
		name: "system and developer",
		body: `{"model":"m","instructions":"be terse","input":[{"type":"message","role":"system","content":[{"type":"input_text","text":"you are helpful"}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"prefer bullets"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
		want: map[string]string{
			"anthropic/buffered":   `{"model":"m","max_tokens":20000,"system":"be terse\n\nyou are helpful\n\nprefer bullets","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"stream":true}`,
			"anthropic/streamed":   `{"model":"m","max_tokens":20000,"system":"be terse\n\nyou are helpful\n\nprefer bullets","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],"stream":true}`,
			"deepseek/buffered":    `{"messages":[{"role":"system","content":"be terse"},{"role":"system","content":"you are helpful"},{"role":"developer","content":"prefer bullets"},{"role":"user","content":"hi"}],"model":"m","stream":false}`,
			"deepseek/streamed":    `{"messages":[{"role":"system","content":"be terse"},{"role":"system","content":"you are helpful"},{"role":"developer","content":"prefer bullets"},{"role":"user","content":"hi"}],"model":"m","stream":true}`,
			"gemini/buffered":      `{"model":"m","input":[{"type":"user_input","content":[{"type":"text","text":"hi"}]}],"system_instruction":"be terse\n\nyou are helpful\n\nprefer bullets","stream":false}`,
			"gemini/streamed":      `{"model":"m","input":[{"type":"user_input","content":[{"type":"text","text":"hi"}]}],"system_instruction":"be terse\n\nyou are helpful\n\nprefer bullets","stream":true}`,
			"openai/buffered":      `{"model":"m","instructions":"be terse","input":[{"type":"message","role":"system","content":[{"type":"input_text","text":"you are helpful"}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"prefer bullets"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`,
			"openai/streamed":      `{"input":[{"type":"message","role":"system","content":[{"type":"input_text","text":"you are helpful"}]},{"type":"message","role":"developer","content":[{"type":"input_text","text":"prefer bullets"}]},{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"instructions":"be terse","model":"m","stream":true}`,
			"openai-chat/buffered": `{"messages":[{"role":"system","content":"be terse"},{"role":"system","content":"you are helpful"},{"role":"developer","content":"prefer bullets"},{"role":"user","content":"hi"}],"model":"m","stream":false}`,
			"openai-chat/streamed": `{"messages":[{"role":"system","content":"be terse"},{"role":"system","content":"you are helpful"},{"role":"developer","content":"prefer bullets"},{"role":"user","content":"hi"}],"model":"m","stream":true,"stream_options":{"include_usage":true}}`,
		},
	},
	{
		name: "tools and results",
		body: `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}],"encrypted_content":"sig"},{"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{\"location\":\"Rio\"}"},{"type":"function_call_output","call_id":"c1","output":"22C"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"It is 22C."}]}],"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}],"tool_choice":"auto","parallel_tool_calls":false,"max_output_tokens":256}`,
		want: map[string]string{
			"anthropic/buffered":   `{"model":"m","max_tokens":256,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"assistant","content":[{"type":"thinking","thinking":"thinking","signature":"sig"},{"type":"tool_use","id":"c1","name":"get_weather","input":{"location":"Rio"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"22C"}]},{"role":"assistant","content":[{"type":"text","text":"It is 22C."}]}],"tools":[{"name":"get_weather","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true},"stream":true}`,
			"anthropic/streamed":   `{"model":"m","max_tokens":256,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]},{"role":"assistant","content":[{"type":"thinking","thinking":"thinking","signature":"sig"},{"type":"tool_use","id":"c1","name":"get_weather","input":{"location":"Rio"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"22C"}]},{"role":"assistant","content":[{"type":"text","text":"It is 22C."}]}],"tools":[{"name":"get_weather","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true},"stream":true}`,
			"deepseek/buffered":    `{"max_tokens":256,"messages":[{"role":"user","content":"hi"},{"role":"assistant","reasoning_content":"sig","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"Rio\"}"}}]},{"role":"tool","content":"22C","tool_call_id":"c1"},{"role":"assistant","content":"It is 22C."}],"model":"m","parallel_tool_calls":false,"stream":false,"tool_choice":"auto","tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`,
			"deepseek/streamed":    `{"max_tokens":256,"messages":[{"role":"user","content":"hi"},{"role":"assistant","reasoning_content":"sig","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"Rio\"}"}}]},{"role":"tool","content":"22C","tool_call_id":"c1"},{"role":"assistant","content":"It is 22C."}],"model":"m","parallel_tool_calls":false,"stream":true,"tool_choice":"auto","tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`,
			"gemini/buffered":      `{"model":"m","input":[{"type":"user_input","content":[{"type":"text","text":"hi"}]},{"type":"thought","signature":"sig"},{"type":"function_call","id":"c1","name":"get_weather","arguments":{"location":"Rio"}},{"type":"function_result","name":"get_weather","call_id":"c1","result":"22C"},{"type":"model_output","content":[{"type":"text","text":"It is 22C."}]}],"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}],"generation_config":{"max_output_tokens":256,"tool_choice":"auto"},"stream":false}`,
			"gemini/streamed":      `{"model":"m","input":[{"type":"user_input","content":[{"type":"text","text":"hi"}]},{"type":"thought","signature":"sig"},{"type":"function_call","id":"c1","name":"get_weather","arguments":{"location":"Rio"}},{"type":"function_result","name":"get_weather","call_id":"c1","result":"22C"},{"type":"model_output","content":[{"type":"text","text":"It is 22C."}]}],"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}],"generation_config":{"max_output_tokens":256,"tool_choice":"auto"},"stream":true}`,
			"openai/buffered":      `{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}],"encrypted_content":"sig"},{"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{\"location\":\"Rio\"}"},{"type":"function_call_output","call_id":"c1","output":"22C"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"It is 22C."}]}],"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}],"tool_choice":"auto","parallel_tool_calls":false,"max_output_tokens":256}`,
			"openai/streamed":      `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}],"encrypted_content":"sig"},{"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{\"location\":\"Rio\"}"},{"type":"function_call_output","call_id":"c1","output":"22C"},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"It is 22C."}]}],"max_output_tokens":256,"model":"m","parallel_tool_calls":false,"stream":true,"tool_choice":"auto","tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}]}`,
			"openai-chat/buffered": `{"max_completion_tokens":256,"messages":[{"role":"user","content":"hi"},{"role":"assistant","reasoning_content":"sig","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"Rio\"}"}}]},{"role":"tool","content":"22C","tool_call_id":"c1"},{"role":"assistant","content":"It is 22C."}],"model":"m","parallel_tool_calls":false,"stream":false,"tool_choice":"auto","tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`,
			"openai-chat/streamed": `{"max_completion_tokens":256,"messages":[{"role":"user","content":"hi"},{"role":"assistant","reasoning_content":"sig","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"Rio\"}"}}]},{"role":"tool","content":"22C","tool_call_id":"c1"},{"role":"assistant","content":"It is 22C."}],"model":"m","parallel_tool_calls":false,"stream":true,"stream_options":{"include_usage":true},"tool_choice":"auto","tools":[{"type":"function","function":{"name":"get_weather","parameters":{"type":"object"}}}]}`,
		},
	},
	{
		name: "weather fixture",
		body: `{
  "model": "gpt-5.4-mini",
  "input": [
    {"type":"message","role":"user","content":[{"type":"input_text","text":"What is the weather in the city that hosted the 2016 Summer Olympics? Use celsius."}]}
  ],
  "tools": [
    {"type":"function","name":"get_weather","description":"Get the current weather in a given location",
     "parameters":{"type":"object","properties":{"location":{"type":"string","description":"City name"},"unit":{"type":"string","enum":["celsius","fahrenheit"]}},"required":["location","unit"],"additionalProperties":false},
     "strict":true}
  ],
  "reasoning": {"effort":"medium"}
}
`,
		want: map[string]string{
			"anthropic/buffered": `{"model":"gpt-5.4-mini","max_tokens":20000,"messages":[{"role":"user","content":[{"type":"text","text":"What is the weather in the city that hosted the 2016 Summer Olympics? Use celsius."}]}],"tools":[{"name":"get_weather","description":"Get the current weather in a given location","input_schema":{"type":"object","properties":{"location":{"type":"string","description":"City name"},"unit":{"type":"string","enum":["celsius","fahrenheit"]}},"required":["location","unit"],"additionalProperties":false},"strict":true}],"output_config":{"effort":"medium"},"stream":true}`,
			"anthropic/streamed": `{"model":"gpt-5.4-mini","max_tokens":20000,"messages":[{"role":"user","content":[{"type":"text","text":"What is the weather in the city that hosted the 2016 Summer Olympics? Use celsius."}]}],"tools":[{"name":"get_weather","description":"Get the current weather in a given location","input_schema":{"type":"object","properties":{"location":{"type":"string","description":"City name"},"unit":{"type":"string","enum":["celsius","fahrenheit"]}},"required":["location","unit"],"additionalProperties":false},"strict":true}],"output_config":{"effort":"medium"},"stream":true}`,
			"deepseek/buffered":  `{"messages":[{"role":"user","content":"What is the weather in the city that hosted the 2016 Summer Olympics? Use celsius."}],"model":"gpt-5.4-mini","reasoning_effort":"medium","stream":false,"tools":[{"type":"function","function":{"name":"get_weather","description":"Get the current weather in a given location","parameters":{"type":"object","properties":{"location":{"type":"string","description":"City name"},"unit":{"type":"string","enum":["celsius","fahrenheit"]}},"required":["location","unit"],"additionalProperties":false},"strict":true}}]}`,
			"deepseek/streamed":  `{"messages":[{"role":"user","content":"What is the weather in the city that hosted the 2016 Summer Olympics? Use celsius."}],"model":"gpt-5.4-mini","reasoning_effort":"medium","stream":true,"tools":[{"type":"function","function":{"name":"get_weather","description":"Get the current weather in a given location","parameters":{"type":"object","properties":{"location":{"type":"string","description":"City name"},"unit":{"type":"string","enum":["celsius","fahrenheit"]}},"required":["location","unit"],"additionalProperties":false},"strict":true}}]}`,
			"gemini/buffered":    `{"model":"gpt-5.4-mini","input":[{"type":"user_input","content":[{"type":"text","text":"What is the weather in the city that hosted the 2016 Summer Olympics? Use celsius."}]}],"tools":[{"type":"function","name":"get_weather","description":"Get the current weather in a given location","parameters":{"type":"object","properties":{"location":{"type":"string","description":"City name"},"unit":{"type":"string","enum":["celsius","fahrenheit"]}},"required":["location","unit"],"additionalProperties":false}}],"generation_config":{"thinking_level":"medium","tool_choice":"validated"},"stream":false}`,
			"gemini/streamed":    `{"model":"gpt-5.4-mini","input":[{"type":"user_input","content":[{"type":"text","text":"What is the weather in the city that hosted the 2016 Summer Olympics? Use celsius."}]}],"tools":[{"type":"function","name":"get_weather","description":"Get the current weather in a given location","parameters":{"type":"object","properties":{"location":{"type":"string","description":"City name"},"unit":{"type":"string","enum":["celsius","fahrenheit"]}},"required":["location","unit"],"additionalProperties":false}}],"generation_config":{"thinking_level":"medium","tool_choice":"validated"},"stream":true}`,
			"openai/buffered": `{
  "model": "gpt-5.4-mini",
  "input": [
    {"type":"message","role":"user","content":[{"type":"input_text","text":"What is the weather in the city that hosted the 2016 Summer Olympics? Use celsius."}]}
  ],
  "tools": [
    {"type":"function","name":"get_weather","description":"Get the current weather in a given location",
     "parameters":{"type":"object","properties":{"location":{"type":"string","description":"City name"},"unit":{"type":"string","enum":["celsius","fahrenheit"]}},"required":["location","unit"],"additionalProperties":false},
     "strict":true}
  ],
  "reasoning": {"effort":"medium"}
}
`,
			"openai/streamed":      `{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"What is the weather in the city that hosted the 2016 Summer Olympics? Use celsius."}]}],"model":"gpt-5.4-mini","reasoning":{"effort":"medium"},"stream":true,"tools":[{"type":"function","name":"get_weather","description":"Get the current weather in a given location","parameters":{"type":"object","properties":{"location":{"type":"string","description":"City name"},"unit":{"type":"string","enum":["celsius","fahrenheit"]}},"required":["location","unit"],"additionalProperties":false},"strict":true}]}`,
			"openai-chat/buffered": `{"messages":[{"role":"user","content":"What is the weather in the city that hosted the 2016 Summer Olympics? Use celsius."}],"model":"gpt-5.4-mini","reasoning_effort":"medium","stream":false,"tools":[{"type":"function","function":{"name":"get_weather","description":"Get the current weather in a given location","parameters":{"type":"object","properties":{"location":{"type":"string","description":"City name"},"unit":{"type":"string","enum":["celsius","fahrenheit"]}},"required":["location","unit"],"additionalProperties":false},"strict":true}}]}`,
			"openai-chat/streamed": `{"messages":[{"role":"user","content":"What is the weather in the city that hosted the 2016 Summer Olympics? Use celsius."}],"model":"gpt-5.4-mini","reasoning_effort":"medium","stream":true,"stream_options":{"include_usage":true},"tools":[{"type":"function","function":{"name":"get_weather","description":"Get the current weather in a given location","parameters":{"type":"object","properties":{"location":{"type":"string","description":"City name"},"unit":{"type":"string","enum":["celsius","fahrenheit"]}},"required":["location","unit"],"additionalProperties":false},"strict":true}}]}`,
		},
	},
}
