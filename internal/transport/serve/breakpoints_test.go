package serve

import (
	"encoding/json"
	"testing"
)

const marked = `{"input":[{"type":"message","role":"user","content":[` +
	`{"type":"input_text","text":"a","prompt_cache_breakpoint":{"mode":"explicit"}},` +
	`{"type":"input_text","text":"b"}]}],` +
	`"prompt_cache_key":"k","prompt_cache_options":{"ttl":"30m"},"model":"openai/gpt-5.5"}`

func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return fields
}

func firstPart(t *testing.T, fields map[string]any) map[string]any {
	t.Helper()
	input, ok := fields["input"].([]any)
	if !ok || len(input) == 0 {
		t.Fatalf("no input items in %v", fields)
	}
	item := input[0].(map[string]any)
	content, ok := item["content"].([]any)
	if !ok || len(content) == 0 {
		t.Fatalf("no content parts in %v", item)
	}
	return content[0].(map[string]any)
}

func TestStripRemovesBreakpointsAndOptions(t *testing.T) {
	fields := decode(t, stripCacheBreakpoints([]byte(marked)))

	if _, present := fields["prompt_cache_options"]; present {
		t.Error("prompt_cache_options survived")
	}
	if fields["prompt_cache_key"] != "k" {
		t.Errorf("prompt_cache_key = %v, want k: implicit caching is a separate mechanism", fields["prompt_cache_key"])
	}
	if fields["model"] != "openai/gpt-5.5" {
		t.Errorf("model = %v, want openai/gpt-5.5", fields["model"])
	}

	part := firstPart(t, fields)
	if _, present := part["prompt_cache_breakpoint"]; present {
		t.Error("prompt_cache_breakpoint survived")
	}
	if part["text"] != "a" {
		t.Errorf("text = %v, want a", part["text"])
	}
}

// A request with nothing to strip must travel as the bytes it arrived in, so an
// unmarked request cannot be told apart from one that never passed through here.
func TestStripLeavesUnmarkedBodyByteIdentical(t *testing.T) {
	for name, body := range map[string]string{
		"string content": `{"input":[{"type":"message","role":"user","content":"hello"}],"model":"m"}`,
		"parts, no mark": `{"input":[{"type":"message","content":[{"type":"input_text","text":"a"}]}]}`,
		"key only":       `{"input":[],"prompt_cache_key":"k"}`,
		"not an object":  `[1,2,3]`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := string(stripCacheBreakpoints([]byte(body))); got != body {
				t.Errorf("body changed\n got: %s\nwant: %s", got, body)
			}
		})
	}
}

func TestBreakpointsDisabledOnlyOnExplicitFalse(t *testing.T) {
	for value, want := range map[string]bool{
		"false": true,
		"true":  false,
		"":      false,
		"off":   false,
		"0":     false,
	} {
		if got := breakpointsDisabled(value); got != want {
			t.Errorf("breakpointsDisabled(%q) = %v, want %v", value, got, want)
		}
	}
}
