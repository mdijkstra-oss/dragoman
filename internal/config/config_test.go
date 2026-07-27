package config_test

import (
	"strings"
	"testing"

	"github.com/matthijn/dragoman/internal/base"
	"github.com/matthijn/dragoman/internal/config"
)

// protocols is the set the real catalog speaks. It is restated here rather than
// imported because catalog imports config, and a test that reached back for it
// would invert the dependency it is checking.
var protocols = []string{
	"anthropic-messages",
	"gemini-interactions",
	"openai-chat-completions",
	"openai-completions",
	"openai-responses",
}

// TestDefaultTableIsTheShippedBackends pins the embedded table to the services
// dragoman answered for before it was a file. The migration from a Go map to
// YAML is behaviour-preserving or it is a bug, and this is where that is
// decided.
func TestDefaultTableIsTheShippedBackends(t *testing.T) {
	table, err := config.Parse(config.Default, protocols)
	if err != nil {
		t.Fatalf("the shipped table does not parse: %v", err)
	}

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
			Endpoint: "https://api.deepseek.com", KeyEnv: "DEEPSEEK_API_KEY", NoSchema: true,
		},
		"openrouter": {
			Name: "openrouter", Dialect: "openai-completions",
			Endpoint: "https://openrouter.ai/api/v1", KeyEnv: "OPENROUTER_API_KEY",
		},
		"openai-chat": {
			Name: "openai-chat", Dialect: "openai-chat-completions",
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

// TestConfigReplacesTheTable covers the property that makes a config readable:
// the file in front of you is the whole answer to what an instance serves.
func TestConfigReplacesTheTable(t *testing.T) {
	table, err := config.Parse([]byte(`
groq:
  endpoint: https://api.groq.com/openai/v1
  protocol: openai-completions
  auth: GROQ_API_KEY
`), protocols)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(table) != 1 {
		t.Fatalf("serves %d services, want only the one named", len(table))
	}
	if _, served := table["openai"]; served {
		t.Error("a shipped service survived a config that did not name it")
	}
	if table["groq"].KeyEnv != "GROQ_API_KEY" {
		t.Errorf("auth = %q, want the environment variable name", table["groq"].KeyEnv)
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

// TestUnsetCredentialIsNotALoadFailure covers the rule that lets an instance
// serving six providers start with the one key its operator holds.
func TestUnsetCredentialIsNotALoadFailure(t *testing.T) {
	t.Setenv("A_KEY_NOBODY_SET", "")
	table, err := config.Parse([]byte(`
lonely:
  endpoint: https://e
  protocol: openai-responses
  auth: A_KEY_NOBODY_SET
`), protocols)
	if err != nil {
		t.Fatalf("an unset variable refused to load: %v", err)
	}
	if table["lonely"].KeyEnv != "A_KEY_NOBODY_SET" {
		t.Error("the table did not keep the variable name")
	}
}
