package pipe_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/matthijn/dragoman/internal/base"
	"github.com/matthijn/dragoman/internal/catalog"
	"github.com/matthijn/dragoman/internal/core"
	"github.com/matthijn/dragoman/internal/transport/pipe"
)

// recorded is a real executor reading a response captured from a live backend.
type recorded struct{ dir string }

func (r recorded) Do(_ context.Context, _ core.Call) (json.RawMessage, error) {
	return os.ReadFile(filepath.Join(r.dir, "buffered.json"))
}

func (r recorded) Stream(_ context.Context, _ core.Call, onFrame func(base.Frame) error) error {
	body, err := os.ReadFile(filepath.Join(r.dir, "stream.sse"))
	if err != nil {
		return err
	}
	return core.ReadFrames(bytes.NewReader(body), onFrame)
}

func request(t *testing.T, streaming bool) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "weather.json"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if !streaming {
		return body
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	decoded["stream"] = true
	streamed, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return streamed
}

func run(t *testing.T, opts core.Options, body []byte) (string, string, int) {
	t.Helper()
	t.Setenv(catalog.Presets()[opts.Preset].KeyEnv, "test-key")
	out, errs := &bytes.Buffer{}, &bytes.Buffer{}
	streams := pipe.Streams{In: bytes.NewReader(body), Out: out, Err: errs}
	executor := recorded{dir: filepath.Join("..", "..", "..", "testdata", "fixtures", opts.Preset)}
	code := pipe.Run(context.Background(), catalog.New(), opts, streams, executor, pipe.Report{})
	return out.String(), errs.String(), code
}

// TestStreamIsHonouredFromTheBody covers stream as an output setting: it
// decides what the caller receives, not how dragoman talks to the backend.
func TestStreamIsHonouredFromTheBody(t *testing.T) {
	stdout, _, code := run(t, core.Options{Preset: "anthropic"}, request(t, true))
	if code != int(core.ExitOK) {
		t.Fatalf("exit = %d, want 0", code)
	}
	lines := nonEmptyLines(stdout)
	if len(lines) < 5 {
		t.Fatalf("events = %d, want the whole lifecycle", len(lines))
	}
	for index, line := range lines {
		var event base.Event
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("line %d is not one JSON object: %v", index, err)
		}
		if event.Type == "" {
			t.Fatalf("line %d has no event type", index)
		}
	}
	var first base.Event
	_ = json.Unmarshal([]byte(lines[0]), &first)
	if first.Type != base.EventCreated {
		t.Errorf("first event = %q, want %q", first.Type, base.EventCreated)
	}
	var last base.Event
	_ = json.Unmarshal([]byte(lines[len(lines)-1]), &last)
	if !base.IsTerminal(last) {
		t.Errorf("last event = %q, want a terminal event", last.Type)
	}
}

// TestNoStreamForcesOneBody covers the flag that exists only to force the mode
// from a shell without editing JSON.
func TestNoStreamForcesOneBody(t *testing.T) {
	buffered := false
	stdout, _, code := run(t, core.Options{Preset: "anthropic", Stream: &buffered}, request(t, true))
	if code != int(core.ExitOK) {
		t.Fatalf("exit = %d, want 0", code)
	}
	lines := nonEmptyLines(stdout)
	if len(lines) != 1 {
		t.Fatalf("lines = %d, want exactly one buffered body", len(lines))
	}
	var resp base.Response
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("body is not a response: %v", err)
	}
	if resp.Object != base.ObjectResponse {
		t.Errorf("object = %q, want %q", resp.Object, base.ObjectResponse)
	}
	if len(base.FindCalls(resp)) != 1 {
		t.Errorf("calls = %d, want 1", len(base.FindCalls(resp)))
	}
	if strings.Contains(stdout, `"sequence_number"`) {
		t.Error("a buffered run emitted events")
	}
}

// TestDefaultIsBuffered covers the default: one body on stdout, written once,
// when the answer is complete.
func TestDefaultIsBuffered(t *testing.T) {
	stdout, _, code := run(t, core.Options{Preset: "openai"}, request(t, false))
	if code != int(core.ExitOK) {
		t.Fatalf("exit = %d, want 0", code)
	}
	if len(nonEmptyLines(stdout)) != 1 {
		t.Fatalf("lines = %d, want one buffered body", len(nonEmptyLines(stdout)))
	}
}

// TestDropsGoToStderr keeps the answer on stdout usable while the drop stays
// loud. Silence is not an option, but neither is corrupting the payload.
func TestDropsGoToStderr(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],"temperature":0.7}`)
	stdout, stderr, code := run(t, core.Options{Preset: "anthropic"}, body)
	if code != int(core.ExitOK) {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stderr, "temperature") {
		t.Errorf("stderr does not report the drop: %q", stderr)
	}
	if strings.Contains(stdout, "dropped") {
		t.Error("a drop leaked into stdout")
	}
}

// TestNoExecutePrintsTheTranslation covers the whole pipeline minus the
// network.
func TestNoExecutePrintsTheTranslation(t *testing.T) {
	stdout, _, code := run(t, core.Options{Preset: "gemini", NoExecute: true}, request(t, false))
	if code != int(core.ExitOK) {
		t.Fatalf("exit = %d, want 0", code)
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(stdout), &sent); err != nil {
		t.Fatalf("output is not a request body: %v", err)
	}
	if _, present := sent["input"]; !present {
		t.Error("translated request has no input")
	}
}

// TestRawReturnsTheBackendsOwnBytes is how a mapper bug is told apart from a
// provider quirk.
func TestRawReturnsTheBackendsOwnBytes(t *testing.T) {
	stdout, _, code := run(t, core.Options{Preset: "deepseek", Raw: true}, request(t, false))
	if code != int(core.ExitOK) {
		t.Fatalf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, `"chat.completion"`) {
		t.Errorf("raw output was translated: %q", firstBytes(stdout))
	}
}

// TestMissingCredentialIsAUsageFailure keeps secrets out of argv: the only
// place a key can come from is the environment, and its absence is dragoman's
// failure rather than the backend's.
func TestMissingCredentialIsAUsageFailure(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	out, errs := &bytes.Buffer{}, &bytes.Buffer{}
	streams := pipe.Streams{In: bytes.NewReader(request(t, false)), Out: out, Err: errs}
	code := pipe.Run(context.Background(), catalog.New(), core.Options{Preset: "anthropic"},
		streams, recorded{}, pipe.Report{})
	if code != int(core.ExitUsage) {
		t.Errorf("exit = %d, want %d", code, core.ExitUsage)
	}
	if !strings.Contains(errs.String(), "ANTHROPIC_API_KEY") {
		t.Errorf("stderr does not name the variable: %q", errs.String())
	}
}

func nonEmptyLines(text string) []string {
	lines := []string{}
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}

func firstBytes(text string) string {
	if len(text) > 120 {
		return text[:120]
	}
	return text
}
