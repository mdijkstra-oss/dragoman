package main

import (
	"strings"
	"testing"
)

func TestParseInvocation(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantPrompt string
		wantError  string
	}{
		{name: "string", body: `{"model":"gpt-5.4","input":"hello"}`, wantPrompt: "hello"},
		{name: "message", body: `{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`, wantPrompt: "hello"},
		{name: "empty inert fields", body: `{"model":"gpt-5.4","input":"hello","tools":[],"store":false,"metadata":{},"include":null}`, wantPrompt: "hello"},
		{name: "history", body: `{"model":"gpt-5.4","input":[{"role":"user","content":"one"},{"role":"assistant","content":"two"}]}`, wantError: "only system"},
		{name: "assistant", body: `{"model":"gpt-5.4","input":[{"role":"assistant","content":"hello"}]}`, wantError: "only system"},
		{name: "image", body: `{"model":"gpt-5.4","input":[{"role":"user","content":[{"type":"input_image","image_url":"x"}]}]}`, wantError: "image_url"},
		{name: "message metadata", body: `{"model":"gpt-5.4","input":[{"role":"user","content":"hello","id":"msg_1"}]}`, wantError: "input[0].id"},
		{name: "part metadata", body: `{"model":"gpt-5.4","input":[{"role":"user","content":[{"type":"input_text","text":"hello","future":1}]}]}`, wantError: "content[0].future"},
		{name: "tool", body: `{"model":"gpt-5.4","input":"hello","tools":[{"type":"function","name":"f"}]}`, wantError: "caller tools"},
		{name: "persistence", body: `{"model":"gpt-5.4","input":"hello","store":true}`, wantError: "store"},
		{name: "previous response", body: `{"model":"gpt-5.4","input":"hello","previous_response_id":"resp_1"}`, wantError: "previous_response_id"},
		{name: "token limit", body: `{"model":"gpt-5.4","input":"hello","max_output_tokens":10}`, wantError: "max_output_tokens"},
		{name: "reasoning summary", body: `{"model":"gpt-5.4","input":"hello","reasoning":{"effort":"high","summary":"auto"}}`, wantError: "summaries"},
		{name: "unknown nested", body: `{"model":"gpt-5.4","input":"hello","reasoning":{"effort":"high","future":1}}`, wantError: "reasoning.future"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			call, _, err := parseInvocation([]byte(testCase.body))
			if testCase.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), testCase.wantError) {
					t.Fatalf("error = %v, want one containing %q", err, testCase.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if call.Prompt != testCase.wantPrompt {
				t.Fatalf("prompt = %q, want %q", call.Prompt, testCase.wantPrompt)
			}
		})
	}
}

func TestInvocationMappings(t *testing.T) {
	body := `{"model":"gpt-5.4","instructions":"line 1\n\"quoted\"","input":"hello","stream":true,"reasoning":{"effort":"high"},"text":{"format":{"type":"json_schema","name":"answer","strict":true,"schema":{"type":"object"}}}}`
	call, stream, err := parseInvocation([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if !stream || call.Model != "gpt-5.4" || call.Effort != "high" || call.Prompt != "hello" {
		t.Fatalf("mapping: %+v, stream %v", call, stream)
	}
	if call.Instructions != "line 1\n\"quoted\"" || string(call.Schema) != `{"type":"object"}` {
		t.Fatalf("instructions/schema: %+v", call)
	}
	if got := tomlString(call.Instructions); got != `"line 1\n\"quoted\""` {
		t.Fatalf("TOML string = %s", got)
	}

	plain, _, err := parseInvocation([]byte(`{"model":"gpt-5.4","input":"hello","text":{"format":{"type":"text"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if plain.Instructions != defaultInstructions || len(plain.Schema) != 0 {
		t.Fatalf("plain mapping: %+v", plain)
	}

	contextual, _, err := parseInvocation([]byte(`{"model":"gpt-5.4","instructions":"gateway rules","input":[{"type":"message","role":"system","content":[{"type":"input_text","text":"framework","prompt_cache_breakpoint":{"mode":"explicit"}}]},{"type":"message","role":"system","content":"code definition"},{"type":"message","role":"user","content":"apply the code"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if contextual.Instructions != "gateway rules\n\nframework\n\ncode definition" || contextual.Prompt != "apply the code" {
		t.Fatalf("context mapping: %+v", contextual)
	}
}
