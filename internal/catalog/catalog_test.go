package catalog_test

import (
	"testing"

	"github.com/mdijkstra-oss/dragoman/internal/catalog"
)

const groq = `
groq:
  endpoint: https://api.groq.com/openai/v1
  protocol: openai-completions
  auth: GROQ_API_KEY
`

// TestATableAddsToTheShippedServices covers what an operator gets for writing
// one entry: that service, and the six that were already there.
func TestATableAddsToTheShippedServices(t *testing.T) {
	table, err := catalog.Load([]byte(groq))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if table["groq"].Endpoint != "https://api.groq.com/openai/v1" {
		t.Errorf("groq endpoint = %q, want the one the document names", table["groq"].Endpoint)
	}
	for _, shipped := range []string{"openai", "anthropic", "gemini", "deepseek", "openrouter", "openai-chat"} {
		if _, served := table[shipped]; !served {
			t.Errorf("%q stopped being served because a table named something else", shipped)
		}
	}
	if len(table) != len(catalog.Presets())+1 {
		t.Errorf("serves %d services, want the shipped ones and groq", len(table))
	}
}

// TestANameInBothIsTheDocuments covers pointing a shipped name somewhere else,
// which is the only way to reach a proxy or a gateway under the name callers
// already send.
func TestANameInBothIsTheDocuments(t *testing.T) {
	table, err := catalog.Load([]byte(`
openai:
  endpoint: https://gateway.internal/v1
  protocol: openai-responses
  auth: GATEWAY_KEY
`))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	openai := table["openai"]
	if openai.Endpoint != "https://gateway.internal/v1" {
		t.Errorf("endpoint = %q, want the one the document names", openai.Endpoint)
	}
	if openai.KeyEnv != "GATEWAY_KEY" {
		t.Errorf("auth = %q, want the one the document names", openai.KeyEnv)
	}
	if len(table) != len(catalog.Presets()) {
		t.Errorf("serves %d services, want the shipped count with one redefined", len(table))
	}
}

// TestOverrideStandsForTheWholeTable covers a deployment locked to an approved
// list: what the document names is served and nothing else is.
func TestOverrideStandsForTheWholeTable(t *testing.T) {
	table, err := catalog.Load([]byte("mode: override\n" + groq))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if len(table) != 1 {
		t.Fatalf("serves %d services, want only the one named", len(table))
	}
	if _, served := table["openai"]; served {
		t.Error("a shipped service survived a table that stands for the whole of one")
	}
}

// TestLoadingTwiceDoesNotAccumulate covers the built-in table being handed out
// fresh: a merge that wrote into it would leak the first document's services
// into the second.
func TestLoadingTwiceDoesNotAccumulate(t *testing.T) {
	if _, err := catalog.Load([]byte(groq)); err != nil {
		t.Fatalf("load: %v", err)
	}
	if _, served := catalog.Presets()["groq"]; served {
		t.Error("a loaded table wrote itself into the shipped one")
	}
}
