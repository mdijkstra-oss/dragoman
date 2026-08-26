package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func parse(t *testing.T, body string) wireRequest {
	t.Helper()
	var req wireRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	return req
}

func TestUnservable(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"plain", `{"stream":true,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`, ""},
		{"history with one user turn", `{"stream":true,"messages":[{"role":"assistant","content":"a"},{"role":"user","content":"b"}]}`, ""},
		{"tools", `{"stream":true,"tools":[{"name":"f"}],"messages":[{"role":"user","content":"hi"}]}`, "no tools"},
		{"empty tools", `{"stream":true,"tools":[],"messages":[{"role":"user","content":"hi"}]}`, ""},
		{"tool choice", `{"stream":true,"tool_choice":{"type":"auto"},"messages":[{"role":"user","content":"hi"}]}`, "tool_choice"},
		{"two turns", `{"stream":true,"messages":[{"role":"user","content":"a"},{"role":"assistant","content":"b"},{"role":"user","content":"c"}]}`, "2 user turns"},
		{"tool result", `{"stream":true,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"y"}]}]}`, "tool exchange"},
		{"no user", `{"stream":true,"messages":[{"role":"assistant","content":"a"}]}`, "no user message"},
		{"buffered", `{"stream":false,"messages":[{"role":"user","content":"hi"}]}`, "stream: true"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := unservable(parse(t, c.body))
			if c.want == "" && got != "" {
				t.Fatalf("refused: %s", got)
			}
			if c.want != "" && !strings.Contains(got, c.want) {
				t.Fatalf("want a refusal naming %q, got %q", c.want, got)
			}
		})
	}
}

func TestInvocation(t *testing.T) {
	req := parse(t, `{"model":"claude-sonnet-5","system":"be brief","stream":true,
		"output_config":{"effort":"low","format":{"type":"json_schema","schema":{"type":"object"}}},
		"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	call, err := invocation(req)
	if err != nil {
		t.Fatal(err)
	}
	if call.Model != "claude-sonnet-5" || call.System != "be brief" || call.Effort != "low" {
		t.Fatalf("flags not carried: %+v", call)
	}
	if string(call.Schema) != `{"type":"object"}` {
		t.Fatalf("schema not carried: %s", call.Schema)
	}
	want := `{"type":"user","message":{"role":"user","content":[{"type":"text","text":"hi"}]}}` + "\n"
	if string(call.Stdin) != want {
		t.Fatalf("stdin:\n%s\nwant:\n%s", call.Stdin, want)
	}
}

func TestChildEnvDropsKeys(t *testing.T) {
	got := childEnv([]string{
		"PATH=/bin", "ANTHROPIC_API_KEY=sk", "ANTHROPIC_BASE_URL=x",
		"CLAUDE_CODE_USE_BEDROCK=1", "CLAUDE_CODE_USE_VERTEX=1", "CLAUDE_CONFIG_DIR=/c",
	})
	if strings.Join(got, " ") != "PATH=/bin CLAUDE_CONFIG_DIR=/c" {
		t.Fatalf("got %v", got)
	}
}

func TestReadFrames(t *testing.T) {
	stream := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`not json`,
		`{"type":"stream_event","event":{"type":"message_start"}}`,
		`{"type":"rate_limit_event"}`,
		`{"type":"stream_event","event":{"type":"message_stop"}}`,
		`{"type":"result","subtype":"success","is_error":false}`,
	}, "\n")
	seen := []string{}
	outcome, err := readFrames(strings.NewReader(stream), func(event json.RawMessage) error {
		seen = append(seen, string(event))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0] != `{"type":"message_start"}` {
		t.Fatalf("events: %v", seen)
	}
	if !outcome.Seen || outcome.Failed {
		t.Fatalf("outcome: %+v", outcome)
	}
}

func TestReadFramesFailure(t *testing.T) {
	stream := `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"Not logged in"}`
	outcome, err := readFrames(strings.NewReader(stream), func(json.RawMessage) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Failed || outcome.Status != "error_during_execution" || outcome.Message != "Not logged in" {
		t.Fatalf("outcome: %+v", outcome)
	}
	status := 529
	if got := resultOutcome(frame{IsError: true, APIErrorStatus: &status}).Status; got != "HTTP 529" {
		t.Fatalf("status: %s", got)
	}
}
