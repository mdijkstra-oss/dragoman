package config_test

import (
	"strings"
	"testing"

	"github.com/mdijkstra-oss/dragoman/internal/base"
	"github.com/mdijkstra-oss/dragoman/internal/config"
)

// protocols is the set the real catalog speaks. It is restated here rather than
// imported because catalog imports config, and a test that reached back for it
// would invert the dependency it is checking.
var protocols = []string{
	"anthropic-messages",
	"gemini-interactions",
	"openai-completions",
	"openai-responses",
}

// TestDefaultTableIsTheShippedBackends pins the embedded table to the services
// dragoman answered for before it was a file. The migration from a Go map to
// YAML is behaviour-preserving or it is a bug, and this is where that is
// decided.
func TestDefaultTableIsTheShippedBackends(t *testing.T) {
	parsed, err := config.Parse(config.Default, protocols)
	if err != nil {
		t.Fatalf("the shipped table does not parse: %v", err)
	}
	table := parsed.Services

	want := map[string]base.Backend{
		"openai": {
			Name: "openai", Dialect: "openai-responses",
			Endpoint: "https://api.openai.com/v1", KeyEnv: "OPENAI_API_KEY",
		},
		"anthropic": {
			Name: "anthropic", Dialect: "anthropic-messages",
			Endpoint: "https://api.anthropic.com/v1", KeyEnv: "ANTHROPIC_API_KEY",
		},
		"gemini": {
			Name: "gemini", Dialect: "gemini-interactions",
			Endpoint: "https://generativelanguage.googleapis.com/v1beta", KeyEnv: "GEMINI_API_KEY",
		},
		"deepseek": {
			Name: "deepseek", Dialect: "openai-completions",
			Endpoint: "https://api.deepseek.com", KeyEnv: "DEEPSEEK_API_KEY",
			SchemaAsPrompt: true, MaxOutputTokensAs: "max_tokens",
		},
		"openrouter": {
			Name: "openrouter", Dialect: "openai-completions",
			Endpoint: "https://openrouter.ai/api/v1", KeyEnv: "OPENROUTER_API_KEY",
		},
		"openai-chat": {
			Name: "openai-chat", Dialect: "openai-completions",
			Endpoint: "https://api.openai.com/v1", KeyEnv: "OPENAI_API_KEY",
		},
	}

	if len(table) != len(want) {
		t.Errorf("table serves %d services, want %d", len(table), len(want))
	}
	for name, expected := range want {
		t.Run(name, func(t *testing.T) {
			got, served := table[name]
			if !served {
				t.Fatalf("%q is not served", name)
			}
			if got != expected {
				t.Errorf("got  %+v\nwant %+v", got, expected)
			}
		})
	}
}

// TestParseReturnsOnlyTheDocument pins the boundary the merge is built on: this
// is the document and nothing else, and what an instance serves is that laid
// over the built-in table.
func TestParseReturnsOnlyTheDocument(t *testing.T) {
	parsed, err := config.Parse([]byte(`
groq:
  endpoint: https://api.groq.com/openai/v1
  protocol: openai-completions
  auth: GROQ_API_KEY
`), protocols)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	table := parsed.Services
	if len(table) != 1 {
		t.Fatalf("parsed %d services, want only the one named", len(table))
	}
	if _, present := table["openai"]; present {
		t.Error("a shipped service came back from a document that did not name it")
	}
	if table["groq"].KeyEnv != "GROQ_API_KEY" {
		t.Errorf("auth = %q, want the environment variable name", table["groq"].KeyEnv)
	}
}

func TestOmitDefaultMaxOutputTokensIsResponsesOnly(t *testing.T) {
	responses := []byte(`
codex:
  endpoint: http://127.0.0.1:8083/v1
  protocol: openai-responses
  auth: CODEX_BRIDGE_KEY
  omit_default_max_output_tokens: true
`)
	parsed, err := config.Parse(responses, protocols)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Services["codex"].OmitDefaultMaxOutputTokens {
		t.Fatal("omit_default_max_output_tokens was not carried to the backend")
	}

	notResponses := strings.Replace(string(responses), "openai-responses", "anthropic-messages", 1)
	if _, err := config.Parse([]byte(notResponses), protocols); err == nil || !strings.Contains(err.Error(), "omit_default_max_output_tokens") {
		t.Fatalf("non-Responses error = %v", err)
	}
}

// TestModeIsReadFromTheDocument covers the choice between adding to the built-in
// services and standing for all of them, which is the document's to make.
func TestModeIsReadFromTheDocument(t *testing.T) {
	entry := "groq:\n  endpoint: https://e\n  protocol: openai-completions\n  auth: GROQ_API_KEY\n"

	cases := []struct {
		name     string
		document string
		want     config.Mode
	}{
		{name: "absent", document: entry, want: config.ModeAppend},
		{name: "append", document: "mode: append\n" + entry, want: config.ModeAppend},
		{name: "override", document: "mode: override\n" + entry, want: config.ModeOverride},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			parsed, err := config.Parse([]byte(testCase.document), protocols)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if parsed.Mode != testCase.want {
				t.Errorf("mode = %q, want %q", parsed.Mode, testCase.want)
			}
			if _, served := parsed.Services["mode"]; served {
				t.Error("mode was read as a service as well as a mode")
			}
			if len(parsed.Services) != 1 {
				t.Errorf("parsed %d services, want the one named", len(parsed.Services))
			}
		})
	}
}

// TestAnUnreadableModeIsRefused covers the value that is neither: taking it for
// either one would serve a table nobody asked for.
func TestAnUnreadableModeIsRefused(t *testing.T) {
	entry := "groq:\n  endpoint: https://e\n  protocol: openai-completions\n  auth: GROQ_API_KEY\n"

	for _, document := range []string{"mode: replace\n" + entry, "mode: {a: b}\n" + entry} {
		_, err := config.Parse([]byte(document), protocols)
		if err == nil {
			t.Fatalf("a mode dragoman cannot read was accepted:\n%s", document)
		}
		for _, mention := range []string{"mode", "append", "override"} {
			if !strings.Contains(err.Error(), mention) {
				t.Errorf("error does not mention %q: %v", mention, err)
			}
		}
	}
}

// TestFormatIsParsedBackUnchanged covers what dragoman config prints: an
// operator edits that output and passes it back, so a field lost on the way out
// is a service that quietly changes shape.
func TestFormatIsParsedBackUnchanged(t *testing.T) {
	parsed, err := config.Parse(config.Default, protocols)
	if err != nil {
		t.Fatalf("the shipped table does not parse: %v", err)
	}
	table := parsed.Services

	printed, err := config.Format(table)
	if err != nil {
		t.Fatalf("format: %v", err)
	}
	again, err := config.Parse(printed, protocols)
	if err != nil {
		t.Fatalf("printed table does not parse:\n%s\n%v", printed, err)
	}

	if again.Mode != config.ModeOverride {
		t.Errorf("printed table says mode %q, want %q: a printed table is the whole of one",
			again.Mode, config.ModeOverride)
	}
	if len(again.Services) != len(table) {
		t.Fatalf("printed %d services, want %d", len(again.Services), len(table))
	}
	for name, expected := range table {
		if again.Services[name] != expected {
			t.Errorf("%s:\ngot  %+v\nwant %+v", name, again.Services[name], expected)
		}
	}
}

// TestLoadFailuresNameWhatIsWrong covers the failures that must happen at load.
// A table dragoman cannot serve from is not one to discover on the first
// request, which is to say in production.
func TestLoadFailuresNameWhatIsWrong(t *testing.T) {
	cases := []struct {
		name     string
		document string
		mentions []string
	}{
		{
			name:     "unknown protocol lists the ones that exist",
			document: "x:\n  endpoint: https://e\n  protocol: nope\n  auth: K\n",
			mentions: []string{"x", "nope", "anthropic-messages", "openai-responses"},
		},
		{
			name:     "missing protocol",
			document: "x:\n  endpoint: https://e\n  auth: K\n",
			mentions: []string{"x", "protocol"},
		},
		{
			name:     "missing endpoint",
			document: "x:\n  protocol: openai-responses\n  auth: K\n",
			mentions: []string{"x", "endpoint"},
		},
		{
			name:     "missing auth",
			document: "x:\n  endpoint: https://e\n  protocol: openai-responses\n",
			mentions: []string{"x", "auth"},
		},
		{
			name:     "empty table",
			document: "",
			mentions: []string{"no services"},
		},
		{
			name:     "not yaml",
			document: "\tthis: [is not: yaml\n",
			mentions: []string{"config"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := config.Parse([]byte(testCase.document), protocols)
			if err == nil {
				t.Fatal("a table dragoman cannot serve from was accepted")
			}
			for _, mention := range testCase.mentions {
				if !strings.Contains(err.Error(), mention) {
					t.Errorf("error does not mention %q: %v", mention, err)
				}
			}
		})
	}
}

// TestServiceWithoutAuthIsRefused covers the rule that every service names the
// variable holding its key: a service that names none could only fail at the
// call, with no way to tell a missing key from a wrong one.
func TestServiceWithoutAuthIsRefused(t *testing.T) {
	_, err := config.Parse([]byte(`
local:
  endpoint: https://e
  protocol: anthropic-messages
`), protocols)
	if err == nil {
		t.Fatal("a service naming no variable loaded")
	}
	if !strings.Contains(err.Error(), "has no auth") {
		t.Errorf("error does not name the missing auth: %v", err)
	}
}

// TestUnsetCredentialIsNotALoadFailure covers the rule that lets an instance
// serving six providers start with the one key its operator holds.
func TestUnsetCredentialIsNotALoadFailure(t *testing.T) {
	t.Setenv("A_KEY_NOBODY_SET", "")
	parsed, err := config.Parse([]byte(`
lonely:
  endpoint: https://e
  protocol: openai-responses
  auth: A_KEY_NOBODY_SET
`), protocols)
	if err != nil {
		t.Fatalf("an unset variable refused to load: %v", err)
	}
	if parsed.Services["lonely"].KeyEnv != "A_KEY_NOBODY_SET" {
		t.Error("the table did not keep the variable name")
	}
}
