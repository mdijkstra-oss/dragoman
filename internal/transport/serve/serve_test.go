package serve_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/mdijkstra-oss/dragoman/internal/base"
	"github.com/mdijkstra-oss/dragoman/internal/catalog"
	"github.com/mdijkstra-oss/dragoman/internal/core"
	"github.com/mdijkstra-oss/dragoman/internal/transport/pipe"
	"github.com/mdijkstra-oss/dragoman/internal/transport/serve"
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

// counted answers whether a backend was reached at all, which is the half of
// strict mode a status code cannot show.
type counted struct {
	inner recorded
	calls *int
}

func (c counted) Do(ctx context.Context, call core.Call) (json.RawMessage, error) {
	*c.calls++
	return c.inner.Do(ctx, call)
}

func (c counted) Stream(ctx context.Context, call core.Call, onFrame func(base.Frame) error) error {
	*c.calls++
	return c.inner.Stream(ctx, call, onFrame)
}

// captured keeps the call the executor was handed. The forwarded model and the
// body carrying it are settled inside the core, so the call is the only place a
// test can read what the backend was actually asked for.
type captured struct {
	inner recorded
	call  *core.Call
}

func (c captured) Do(ctx context.Context, call core.Call) (json.RawMessage, error) {
	*c.call = call
	return c.inner.Do(ctx, call)
}

func (c captured) Stream(ctx context.Context, call core.Call, onFrame func(base.Frame) error) error {
	*c.call = call
	return c.inner.Stream(ctx, call, onFrame)
}

// answer is one exchange with the listener, read to the end so the test can
// assert on the whole of it.
type answer struct {
	status int
	header http.Header
	body   string
}

// failure is the openai-responses error object every refusal is written as.
type failure struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

// leakSentinel is a credential value no answer may ever contain.
const leakSentinel = "sk-secret-do-not-leak"

// responses is the one route that translates. There is no service in it: a
// request names its service in the model, so every backend is reached here.
const responses = "/responses"

var fixtureServices = []string{"anthropic", "deepseek", "gemini", "openai"}

// TestListenerAddsFramingAndNothingElse is the claim the whole transport rests
// on: the same request answered over HTTP and over the pipe is the same
// answer, so choosing a transport never costs fidelity.
func TestListenerAddsFramingAndNothingElse(t *testing.T) {
	for _, service := range fixtureServices {
		t.Run(service, func(t *testing.T) {
			body := requestBody(t, false)
			server := listen(t, serve.Config{Executor: fixtures(service)})
			answered := send(t, server, http.MethodPost, responses, addressed(t, body, service))

			if answered.status != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", answered.status, answered.body)
			}
			if got := answered.header.Get("Content-Type"); got != "application/json" {
				t.Errorf("content type = %q, want application/json", got)
			}
			piped := pipeBody(t, service, body)
			if !reflect.DeepEqual(decodeJSON(t, "listener", answered.body), decodeJSON(t, "pipe", piped)) {
				t.Errorf("listener and pipe disagree:\n listener: %s\n pipe:     %s", answered.body, piped)
			}
		})
	}
}

// TestStreamIsReframedEventForEvent covers the reframing: the payloads are the
// pipe's NDJSON lines untouched, so an SDK stream parser reads what the pipe
// writes.
func TestStreamIsReframedEventForEvent(t *testing.T) {
	for _, service := range fixtureServices {
		t.Run(service, func(t *testing.T) {
			body := requestBody(t, true)
			server := listen(t, serve.Config{Executor: fixtures(service)})
			answered := send(t, server, http.MethodPost, responses, addressed(t, body, service))

			if answered.status != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", answered.status, answered.body)
			}
			if got := answered.header.Get("Content-Type"); got != "text/event-stream" {
				t.Fatalf("content type = %q, want text/event-stream", got)
			}
			if !strings.HasSuffix(answered.body, "\n\n") {
				t.Errorf("the last event is not terminated by a blank line: %q", lastBytes(answered.body))
			}
			payloads := dataPayloads(t, answered.body)
			want := nonEmptyLines(pipeBody(t, service, body))
			if len(payloads) != len(want) {
				t.Fatalf("events = %d, want %d", len(payloads), len(want))
			}
			for index := range want {
				if payloads[index] != want[index] {
					t.Fatalf("event %d differs:\n sse:    %s\n ndjson: %s", index, payloads[index], want[index])
				}
			}
		})
	}
}

// TestTheModelNamesTheService covers routing whole: the service is the model's
// first segment, the backend is handed what is left of the name, and a model
// that addresses nothing is refused rather than sent somewhere plausible. The
// split is on the first slash only, which is what keeps a provider whose own
// identifiers are paths reachable at all.
func TestTheModelNamesTheService(t *testing.T) {
	table := catalog.Presets()
	cases := []struct {
		name      string
		path      string
		model     string
		fixture   string
		status    int
		endpoint  string
		forwarded string
		mentions  []string
	}{
		{
			name:      "the first segment is the service and the rest is the model",
			path:      responses,
			model:     "openai/gpt-5.5",
			fixture:   "openai",
			status:    http.StatusOK,
			endpoint:  table["openai"].Endpoint,
			forwarded: "gpt-5.5",
		},
		{
			name:      "a nested identifier survives, because only the first slash splits",
			path:      responses,
			model:     "openrouter/anthropic/claude-3",
			fixture:   "deepseek",
			status:    http.StatusOK,
			endpoint:  table["openrouter"].Endpoint,
			forwarded: "anthropic/claude-3",
		},
		{
			name:     "a model naming no service is 404 naming what is served",
			path:     responses,
			model:    "gpt-5.5",
			fixture:  "openai",
			status:   http.StatusNotFound,
			mentions: []string{"gpt-5.5", "anthropic", "openai"},
		},
		{
			name:     "a first segment naming no service is 404 naming what is served",
			path:     responses,
			model:    "nowhere/gpt-5.5",
			fixture:  "openai",
			status:   http.StatusNotFound,
			mentions: []string{"nowhere/gpt-5.5", "anthropic", "openai"},
		},
		{
			name:     "a service in the path is not a route",
			path:     "/openai/responses",
			model:    "openai/gpt-5.5",
			fixture:  "openai",
			status:   http.StatusNotFound,
			mentions: []string{"/openai/responses", "POST /responses"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			call := core.Call{}
			server := listen(t, serve.Config{
				Executor: captured{inner: fixtures(testCase.fixture), call: &call},
			})
			answered := send(t, server, http.MethodPost, testCase.path,
				withModel(t, requestBody(t, false), testCase.model))

			if answered.status != testCase.status {
				t.Fatalf("status = %d, want %d: %s", answered.status, testCase.status, answered.body)
			}
			for _, mention := range testCase.mentions {
				if !strings.Contains(answered.body, mention) {
					t.Errorf("body does not mention %q: %s", mention, answered.body)
				}
			}
			if testCase.status != http.StatusOK {
				assertFailureBody(t, answered.body, "invalid_request_error")
				if call.URL != "" {
					t.Errorf("a backend was reached at %q for a request nothing could resolve", call.URL)
				}
				return
			}
			if !strings.HasPrefix(call.URL, testCase.endpoint) {
				t.Errorf("reached %q, want the service at %q", call.URL, testCase.endpoint)
			}
			if got := modelOf(t, call.Body); got != testCase.forwarded {
				t.Errorf("forwarded model = %q, want %q", got, testCase.forwarded)
			}
		})
	}
}

// TestTheRewriteKeepsEverythingItDidNotName is what the pass-through path costs
// given that the model is always rewritten on it. Every other key reaches the
// backend as the caller's own bytes — nested values included, and the keys the
// base does not model with them — so the body is equivalent rather than merely
// close, and only the top-level key order moves.
//
// The prompt carries the characters a JSON encoder escapes by default. They are
// the ones a rewrite loses quietly: the body stays a valid encoding of the same
// value, so only a byte comparison catches content that arrived edited.
func TestTheRewriteKeepsEverythingItDidNotName(t *testing.T) {
	sent := []byte(`{"model":"openai/gpt-5.5",` +
		`"input":[{"type":"message","role":"user","content":` +
		`[{"type":"input_text","text":"if a < b && c > d then <b>bold</b>"}]}],` +
		`"top_p":0.25,"unmodelled":{"nested":[1,{"deep":true},null]},"metadata":{"team":"acme & co"}}`)

	call := core.Call{}
	server := listen(t, serve.Config{Executor: captured{inner: fixtures("openai"), call: &call}})
	if answered := send(t, server, http.MethodPost, responses, sent); answered.status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", answered.status, answered.body)
	}

	before, after := rawFields(t, sent), rawFields(t, call.Body)
	if !slices.Equal(slices.Sorted(maps.Keys(before)), slices.Sorted(maps.Keys(after))) {
		t.Fatalf("keys = %v, want %v", slices.Sorted(maps.Keys(after)), slices.Sorted(maps.Keys(before)))
	}
	if got := string(after["model"]); got != `"gpt-5.5"` {
		t.Errorf("model = %s, want \"gpt-5.5\"", got)
	}
	for key, value := range before {
		if key == "model" {
			continue
		}
		if string(after[key]) != string(value) {
			t.Errorf("%s = %s, want %s", key, after[key], value)
		}
	}
}

// TestStatusMapping covers each row of the exit-code-to-status table. The
// mapping is what makes a failure actionable: an SDK retries a 429 and an
// operator reads a 500, so a class boundary crossed here is a bug in the field.
func TestStatusMapping(t *testing.T) {
	keyEnv := catalog.Presets()["anthropic"].KeyEnv
	cases := []struct {
		name      string
		method    string
		path      string
		body      []byte
		unset     string
		status    int
		errorType string
		mentions  []string
	}{
		{
			name:   "a translated request is 200",
			method: http.MethodPost,
			path:   responses,
			body:   addressed(t, requestBody(t, false), "anthropic"),
			status: http.StatusOK,
		},
		{
			name:      "an unresolvable model is 404 naming what is served",
			method:    http.MethodPost,
			path:      responses,
			body:      addressed(t, requestBody(t, false), "nowhere"),
			status:    http.StatusNotFound,
			errorType: "invalid_request_error",
			mentions:  []string{"nowhere", "anthropic", "openai"},
		},
		{
			name:      "an unset credential is 500 naming the variable",
			method:    http.MethodPost,
			path:      responses,
			body:      addressed(t, requestBody(t, false), "anthropic"),
			unset:     keyEnv,
			status:    http.StatusInternalServerError,
			errorType: "server_error",
			mentions:  []string{keyEnv},
		},
		{
			name:      "an unparseable body is 400",
			method:    http.MethodPost,
			path:      responses,
			body:      []byte(`{"input":`),
			status:    http.StatusBadRequest,
			errorType: "invalid_request_error",
			mentions:  []string{"parse request"},
		},
		{
			name:      "a wrong method is 405",
			method:    http.MethodGet,
			path:      responses,
			status:    http.StatusMethodNotAllowed,
			errorType: "invalid_request_error",
			mentions:  []string{http.MethodPost},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := listen(t, serve.Config{Executor: fixtures("anthropic")})
			if testCase.unset != "" {
				t.Setenv(testCase.unset, "")
			}
			answered := send(t, server, testCase.method, testCase.path, testCase.body)

			if answered.status != testCase.status {
				t.Fatalf("status = %d, want %d: %s", answered.status, testCase.status, answered.body)
			}
			for _, mention := range testCase.mentions {
				if !strings.Contains(answered.body, mention) {
					t.Errorf("body does not mention %q: %s", mention, answered.body)
				}
			}
			if testCase.errorType == "" {
				assertResponseBody(t, answered.body)
				return
			}
			assertFailureBody(t, answered.body, testCase.errorType)
		})
	}
}

// TestServicesDescribesTheTableWithoutCredentials covers what makes a running
// instance self-describing, and the line it may not cross: a client learns what
// it can address, never what the process holds.
func TestServicesDescribesTheTableWithoutCredentials(t *testing.T) {
	server := listen(t, serve.Config{Executor: fixtures("anthropic")})
	answered := send(t, server, http.MethodGet, "/services", nil)

	if answered.status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", answered.status, answered.body)
	}
	if strings.Contains(answered.body, leakSentinel) {
		t.Fatalf("a credential value reached the answer: %s", answered.body)
	}

	var rows []serve.Service
	if err := json.Unmarshal([]byte(answered.body), &rows); err != nil {
		t.Fatalf("services is not a table: %v", err)
	}
	table := catalog.Presets()
	if len(rows) != len(table) {
		t.Fatalf("services = %d, want %d", len(rows), len(table))
	}
	for name, backend := range table {
		row, listed := findService(rows, name)
		if !listed {
			t.Errorf("service %q is not listed", name)
			continue
		}
		want := serve.Service{
			Name:      name,
			Protocol:  backend.Dialect,
			Endpoint:  backend.Endpoint,
			Auth:      backend.KeyEnv,
			Available: true,
		}
		if row != want {
			t.Errorf("service %q = %+v, want %+v", name, row, want)
		}
	}
	for _, fields := range fieldNames(t, answered.body) {
		if !slices.Equal(fields, []string{"auth", "available", "endpoint", "name", "protocol"}) {
			t.Errorf("a service row carries fields beyond the table: %v", fields)
		}
	}
}

// TestServicesReportsWhichCredentialsAreMissing covers the question a client asks
// before it picks a model: which of these would answer right now. An unset key is
// the one reason a listed service would not, and naming it here is what turns a
// runtime 500 into something checkable up front.
func TestServicesReportsWhichCredentialsAreMissing(t *testing.T) {
	const absent = "GEMINI_API_KEY"

	server := listen(t, serve.Config{Executor: fixtures("anthropic")})
	t.Setenv(absent, "")
	answered := send(t, server, http.MethodGet, "/services", nil)

	var rows []serve.Service
	if err := json.Unmarshal([]byte(answered.body), &rows); err != nil {
		t.Fatalf("services is not a table: %v", err)
	}

	for name, backend := range catalog.Presets() {
		row, listed := findService(rows, name)
		if !listed {
			t.Errorf("service %q is not listed", name)
			continue
		}
		// A service naming no variable holds its own credential, so nothing here
		// can make it unavailable.
		want := backend.KeyEnv != absent
		if row.Available != want {
			t.Errorf("service %q available = %v, want %v", name, row.Available, want)
		}
	}
}

// TestHealthIsLiveness covers the one route that answers without touching a
// backend, which is what a process supervisor polls.
func TestHealthIsLiveness(t *testing.T) {
	server := listen(t, serve.Config{Executor: fixtures("anthropic")})
	answered := send(t, server, http.MethodGet, "/health", nil)
	if answered.status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", answered.status, answered.body)
	}
}

// TestDropsGoToTheLogAndNotTheBody keeps the answer a client can parse while
// the drop stays loud for the operator. On the pipe the caller reads stderr;
// over HTTP there is nobody to read it but the log.
func TestDropsGoToTheLogAndNotTheBody(t *testing.T) {
	logged := &bytes.Buffer{}
	server := listen(t, serve.Config{
		Executor: fixtures("anthropic"),
		Log:      slog.New(slog.NewTextHandler(logged, nil)),
	})
	answered := send(t, server, http.MethodPost, responses, addressed(t, droppableBody(), "anthropic"))

	if answered.status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", answered.status, answered.body)
	}
	for _, leaked := range []string{"temperature", "dropped", "unsupported", "unimplemented"} {
		if strings.Contains(answered.body, leaked) {
			t.Errorf("the drop leaked into the answer as %q: %s", leaked, answered.body)
		}
	}
	if !strings.Contains(logged.String(), "temperature") {
		t.Errorf("the log does not report the drop: %q", logged.String())
	}
}

// TestAnInertDropIsLoggedBelowAWarning keeps the warnings worth reading. A
// field whose absence changed nothing recurs on every request of a kind, and an
// operator who learns to skim those skims the drop that mattered with them.
func TestAnInertDropIsLoggedBelowAWarning(t *testing.T) {
	body := []byte(`{"model":"anthropic/claude-4-6","input":[{"type":"message","role":"user",` +
		`"content":[{"type":"input_text","text":"hi"}]}],` +
		`"text":{"format":{"type":"json_schema","name":"p","strict":true,"schema":{"type":"object"}}}}`)

	warned, everything := &bytes.Buffer{}, &bytes.Buffer{}
	for _, level := range []struct {
		name string
		sink *bytes.Buffer
		opts *slog.HandlerOptions
	}{
		{name: "warn", sink: warned, opts: nil},
		{name: "debug", sink: everything, opts: &slog.HandlerOptions{Level: slog.LevelDebug}},
	} {
		server := listen(t, serve.Config{
			Executor: fixtures("anthropic"),
			Log:      slog.New(slog.NewTextHandler(level.sink, level.opts)),
		})
		if answered := send(t, server, http.MethodPost, responses, body); answered.status != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", answered.status, answered.body)
		}
	}

	if strings.Contains(warned.String(), "text.format") {
		t.Errorf("an inert drop was raised as a warning: %q", warned.String())
	}
	if !strings.Contains(everything.String(), "text.format") {
		t.Errorf("an inert drop went unrecorded at debug: %q", everything.String())
	}
}

// TestStrictRefusesBeforeSending covers the flag's whole job: under strict a
// field that would be dropped is a refused request, and refusing after the
// backend was already paid for would be no refusal at all.
func TestStrictRefusesBeforeSending(t *testing.T) {
	cases := []struct {
		name      string
		streaming bool
	}{
		{name: "buffered", streaming: false},
		{name: "streaming", streaming: true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			calls := 0
			server := listen(t, serve.Config{
				Executor: counted{inner: fixtures("anthropic"), calls: &calls},
				Strict:   true,
			})
			answered := send(t, server, http.MethodPost, responses,
				addressed(t, streamed(t, droppableBody(), testCase.streaming), "anthropic"))

			if answered.status != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", answered.status, answered.body)
			}
			if calls != 0 {
				t.Errorf("backend calls = %d, want none", calls)
			}
			assertFailureBody(t, answered.body, "invalid_request_error")
			if !strings.Contains(answered.body, "temperature") {
				t.Errorf("the refusal does not name the field: %s", answered.body)
			}
		})
	}
}

// TestARequestThatWorkedIsAccountedFor covers the record dragoman writes where
// the money is spent. Nothing else in the deployment knows both which service
// answered and what the answer cost, and the counts have to be the caller's own
// — a number in a log the answer does not carry is a number nobody can check.
// Both framings are here because a streamed answer leaves no response body
// behind, which is exactly where accounting goes missing.
func TestARequestThatWorkedIsAccountedFor(t *testing.T) {
	cases := []struct {
		name      string
		streaming bool
	}{
		{name: "buffered", streaming: false},
		{name: "streaming", streaming: true},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			noAllowlist(t)
			logged := &bytes.Buffer{}
			server := listen(t, serve.Config{
				Executor: fixtures("anthropic"),
				Log:      slog.New(slog.NewJSONHandler(logged, nil)),
			})
			answered := send(t, server, http.MethodPost, responses,
				addressed(t, requestBody(t, testCase.streaming), "anthropic"))

			if answered.status != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", answered.status, answered.body)
			}
			record := theRecord(t, logged.String(), "request served")
			if record["service"] != "anthropic" {
				t.Errorf("service = %v, want anthropic", record["service"])
			}
			if record["model"] != modelOf(t, requestBody(t, testCase.streaming)) {
				t.Errorf("model = %v, want the model the backend was asked for", record["model"])
			}
			if record["status"] != float64(http.StatusOK) {
				t.Errorf("status = %v, want 200", record["status"])
			}
			if _, timed := record["duration_ms"]; !timed {
				t.Errorf("the record does not say how long the exchange took: %v", record)
			}
			assertTokenCounts(t, record, answeredUsage(t, answered))
			if _, carried := record["headers"]; carried {
				t.Errorf("headers were recorded with no allowlist configured: %v", record)
			}
		})
	}
}

// TestAFailedRequestIsNotAccountedFor keeps the record a record of what
// happened. A request that never reached a backend cost nothing, and counting
// it as a served request would put a row of zeroes beside the real ones.
func TestAFailedRequestIsNotAccountedFor(t *testing.T) {
	noAllowlist(t)
	logged := &bytes.Buffer{}
	server := listen(t, serve.Config{
		Executor: fixtures("anthropic"),
		Log:      slog.New(slog.NewJSONHandler(logged, nil)),
	})
	answered := send(t, server, http.MethodPost, responses, addressed(t, requestBody(t, false), "nowhere"))

	if answered.status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", answered.status, answered.body)
	}
	if failed := theRecord(t, logged.String(), "request failed"); failed["status"] != float64(http.StatusNotFound) {
		t.Errorf("the failure line reports status %v, want 404", failed["status"])
	}
	for _, entry := range records(t, logged.String()) {
		if entry["msg"] == "request served" {
			t.Errorf("a refused request was accounted for as served: %v", entry)
		}
	}
}

// TestTheAllowlistDecidesWhichHeadersAreRecorded covers the rule that makes
// forwarding an identity safe to log at all: dragoman records the names it was
// configured to record and no others, so a caller cannot decide what this
// process writes down by attaching a header. Unconfigured is silence rather
// than whatever arrived.
func TestTheAllowlistDecidesWhichHeadersAreRecorded(t *testing.T) {
	cases := []struct {
		name       string
		allow      string
		configured bool
		sent       map[string]string
		want       map[string]string
		absent     []string
	}{
		{
			name:   "an unset variable records nothing",
			sent:   map[string]string{"X-Session-ID": "s-1"},
			absent: []string{"s-1"},
		},
		{
			name:       "two named headers are recorded whole",
			allow:      "X-Session-ID,X-Agent-Route",
			configured: true,
			sent:       map[string]string{"X-Session-ID": "s-1", "X-Agent-Route": "weather/today"},
			want:       map[string]string{"x-session-id": "s-1", "x-agent-route": "weather/today"},
		},
		{
			name:       "a named header the request omits is left out",
			allow:      "X-Session-ID,X-Agent-Route",
			configured: true,
			sent:       map[string]string{"X-Session-ID": "s-1"},
			want:       map[string]string{"x-session-id": "s-1"},
			absent:     []string{"x-agent-route"},
		},
		{
			name:       "a header nobody named is not recorded",
			allow:      "X-Session-ID",
			configured: true,
			sent: map[string]string{
				"X-Session-ID": "s-1",
				"X-Project-ID": "proj-9",
				"X-Trace":      "trace-7",
			},
			want:   map[string]string{"x-session-id": "s-1"},
			absent: []string{"proj-9", "trace-7"},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			allowed := allowlist(t, testCase.allow, testCase.configured)
			logged := &bytes.Buffer{}
			server := listen(t, serve.Config{
				Executor:   fixtures("anthropic"),
				Log:        slog.New(slog.NewJSONHandler(logged, nil)),
				LogHeaders: allowed,
			})
			answered := sendWith(t, server, http.MethodPost, responses,
				addressed(t, requestBody(t, false), "anthropic"), testCase.sent)

			if answered.status != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", answered.status, answered.body)
			}
			record := theRecord(t, logged.String(), "request served")
			recorded, carried := record["headers"].(map[string]any)
			if len(testCase.want) == 0 {
				if carried {
					t.Fatalf("headers were recorded that nothing named: %v", record["headers"])
				}
				return
			}
			if !carried {
				t.Fatalf("the record carries no headers: %v", record)
			}
			for name, value := range testCase.want {
				if recorded[name] != value {
					t.Errorf("header %s = %v, want %q", name, recorded[name], value)
				}
			}
			if len(recorded) != len(testCase.want) {
				t.Errorf("recorded headers = %v, want exactly %v", recorded, testCase.want)
			}
			for _, unwanted := range testCase.absent {
				if strings.Contains(logged.String(), unwanted) {
					t.Errorf("%q reached the log: %s", unwanted, logged.String())
				}
			}
		})
	}
}

// TestAnUnrecordableAllowlistIsRefusedAtLoad covers the half of the rule that
// has to happen before the listener exists. An operator who can name any header
// to log can name the one carrying a key, so a list dragoman will not record
// under is a list it will not start under — found at boot, where the mistake
// is, rather than per request with the record already wrong.
func TestAnUnrecordableAllowlistIsRefusedAtLoad(t *testing.T) {
	seventeen := make([]string, 0, 17)
	for index := range 17 {
		seventeen = append(seventeen, fmt.Sprintf("X-Name-%d", index))
	}
	cases := []struct {
		name     string
		allow    string
		refused  bool
		mentions []string
		want     []string
	}{
		{
			name:     "a credential header is refused by name",
			allow:    "X-Session-ID,Authorization",
			refused:  true,
			mentions: []string{"Authorization"},
		},
		{
			name:     "a credential-shaped X- header is refused by name",
			allow:    "X-Api-Key",
			refused:  true,
			mentions: []string{"X-Api-Key"},
		},
		{
			name:     "more names than a record can carry is refused",
			allow:    strings.Join(seventeen, ","),
			refused:  true,
			mentions: []string{"17", "16"},
		},
		{
			name:  "spelling, spacing and repetition are the operator's own",
			allow: " X-Session-ID , x-session-id,X-Agent-Route ",
			want:  []string{"X-Session-ID", "X-Agent-Route"},
		},
		{
			name:  "an empty variable is an empty allowlist",
			allow: "",
			want:  []string{},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Setenv(serve.LogHeaderVar, testCase.allow)
			names, err := serve.Allowlist()
			if !testCase.refused {
				if err != nil {
					t.Fatalf("%s=%q: %v", serve.LogHeaderVar, testCase.allow, err)
				}
				if !slices.Equal(names, testCase.want) {
					t.Errorf("allowlist = %v, want %v", names, testCase.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s=%q was accepted, want a refusal", serve.LogHeaderVar, testCase.allow)
			}
			if code := core.CodeOf(err); code != core.ExitUsage {
				t.Errorf("exit code = %d, want %d", code, core.ExitUsage)
			}
			for _, mention := range append(testCase.mentions, serve.LogHeaderVar) {
				if !strings.Contains(err.Error(), mention) {
					t.Errorf("the refusal does not mention %q: %v", mention, err)
				}
			}
		})
	}
}

// listen serves the catalog over a real HTTP server, with a key present for
// every service that names a variable, so that a test asserts on translation
// rather than on setup. A service holding its own credential names none.
func listen(t *testing.T, cfg serve.Config) *httptest.Server {
	t.Helper()
	for _, backend := range catalog.Presets() {
		if backend.KeyEnv == "" {
			continue
		}
		t.Setenv(backend.KeyEnv, leakSentinel)
	}
	cfg.Catalog = catalog.New()
	server := httptest.NewServer(serve.New(cfg))
	t.Cleanup(server.Close)
	return server
}

func fixtures(service string) recorded {
	return recorded{dir: filepath.Join("..", "..", "..", "testdata", "fixtures", service)}
}

func send(t *testing.T, server *httptest.Server, method, path string, body []byte) answer {
	t.Helper()
	return sendWith(t, server, method, path, body, nil)
}

// sendWith carries the caller's own headers, which is the whole of what an
// identity forwarded from in front of dragoman looks like from here.
func sendWith(t *testing.T, server *httptest.Server, method, path string, body []byte, headers map[string]string) answer {
	t.Helper()
	outgoing, err := http.NewRequest(method, server.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("building %s %s: %v", method, path, err)
	}
	outgoing.Header.Set("Content-Type", "application/json")
	for name, value := range headers {
		outgoing.Header.Set(name, value)
	}
	response, err := server.Client().Do(outgoing)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer response.Body.Close()
	read, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("reading %s %s: %v", method, path, err)
	}
	return answer{status: response.StatusCode, header: response.Header, body: string(read)}
}

// pipeBody runs the same request through the other transport, which is the
// reference the listener is measured against.
func pipeBody(t *testing.T, service string, body []byte) string {
	t.Helper()
	out, errs := &bytes.Buffer{}, &bytes.Buffer{}
	streams := pipe.Streams{In: bytes.NewReader(body), Out: out, Err: errs}
	code := pipe.Run(context.Background(), catalog.New(), core.Options{Preset: service},
		streams, fixtures(service), pipe.Report{})
	if code != int(core.ExitOK) {
		t.Fatalf("pipe exit = %d: %s", code, errs.String())
	}
	return out.String()
}

func requestBody(t *testing.T, streaming bool) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "testdata", "weather.json"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return streamed(t, body, streaming)
}

func streamed(t *testing.T, body []byte, streaming bool) []byte {
	t.Helper()
	if !streaming {
		return body
	}
	var decoded map[string]any
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	decoded["stream"] = true
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return encoded
}

func droppableBody() []byte {
	return []byte(`{"model":"claude-4-6","input":[{"type":"message","role":"user",` +
		`"content":[{"type":"input_text","text":"hi"}]}],"temperature":0.7}`)
}

// addressed prefixes a service onto the model a body already names. The pipe is
// handed its service directly, so a comparison between the two transports keeps
// the unprefixed fixture for the pipe leg and addresses only the listener's.
func addressed(t *testing.T, body []byte, service string) []byte {
	t.Helper()
	model := modelOf(t, body)
	if model == "" {
		t.Fatalf("fixture names no model to address: %s", body)
	}
	return withModel(t, body, service+"/"+model)
}

// withModel writes the model a request names, which is the whole of how a
// request addresses a service here.
func withModel(t *testing.T, body []byte, model string) []byte {
	t.Helper()
	fields := rawFields(t, body)
	fields["model"] = json.RawMessage(strconv.Quote(model))
	encoded, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return encoded
}

// rawFields reads a body as its top-level keys and their untouched bytes, which
// is how a value is compared without a decode reshaping it first.
func rawFields(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("body is not an object: %v", err)
	}
	return fields
}

func modelOf(t *testing.T, body []byte) string {
	t.Helper()
	var named struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &named); err != nil {
		t.Fatalf("forwarded body is not JSON: %v", err)
	}
	return named.Model
}

// dataPayloads reads a server-sent event stream back into the payloads it
// carries, rejecting any framing beyond one event line and one data line closed
// by a blank line.
func dataPayloads(t *testing.T, stream string) []string {
	t.Helper()
	payloads := []string{}
	for _, event := range strings.Split(stream, "\n\n") {
		if event == "" {
			continue
		}
		head, tail, split := strings.Cut(event, "\n")
		if !split {
			t.Fatalf("event carries no data line: %q", event)
		}
		name, named := strings.CutPrefix(head, "event: ")
		if !named {
			t.Fatalf("event is not introduced by an event line: %q", event)
		}
		payload, framed := strings.CutPrefix(tail, "data: ")
		if !framed {
			t.Fatalf("event is not a data line: %q", event)
		}
		if strings.Contains(payload, "\n") {
			t.Fatalf("event is not terminated by a blank line: %q", event)
		}
		if typed := typeOf(t, payload); typed != name {
			t.Fatalf("event line names %q, payload names %q", name, typed)
		}
		payloads = append(payloads, payload)
	}
	return payloads
}

// typeOf reads the type a payload names. The event line has to agree with it,
// or a client is handed two answers to which event it just received.
func typeOf(t *testing.T, payload string) string {
	t.Helper()
	var named struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal([]byte(payload), &named); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	return named.Type
}

func assertResponseBody(t *testing.T, body string) {
	t.Helper()
	var response base.Response
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatalf("body is not a response: %v", err)
	}
	if response.Object != base.ObjectResponse {
		t.Errorf("object = %q, want %q", response.Object, base.ObjectResponse)
	}
}

func assertFailureBody(t *testing.T, body, wantType string) {
	t.Helper()
	var refused failure
	if err := json.Unmarshal([]byte(body), &refused); err != nil {
		t.Fatalf("body is not an error object: %v", err)
	}
	if refused.Error.Type != wantType {
		t.Errorf("error type = %q, want %q", refused.Error.Type, wantType)
	}
	if refused.Error.Message == "" {
		t.Error("error object carries no message")
	}
}

// records reads the log back as the lines a collector would index, which is
// also the assertion that every one of them is JSON.
func records(t *testing.T, logged string) []map[string]any {
	t.Helper()
	lines := nonEmptyLines(logged)
	entries := make([]map[string]any, 0, len(lines))
	for _, line := range lines {
		entry := map[string]any{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("log line is not JSON: %v: %s", err, line)
		}
		entries = append(entries, entry)
	}
	return entries
}

// theRecord is the one line saying something, and the claim that there is
// exactly one of it: an accounting line written twice is a request billed
// twice.
func theRecord(t *testing.T, logged, message string) map[string]any {
	t.Helper()
	found := []map[string]any{}
	for _, entry := range records(t, logged) {
		if entry["msg"] == message {
			found = append(found, entry)
		}
	}
	if len(found) != 1 {
		t.Fatalf("lines saying %q = %d, want 1: %s", message, len(found), logged)
	}
	return found[0]
}

// answeredUsage reads the counts the caller was given. A streamed answer
// carries them on the snapshot the terminal event holds, a buffered one on the
// body, and the record has to agree with whichever the caller read.
func answeredUsage(t *testing.T, answered answer) base.Usage {
	t.Helper()
	if answered.header.Get("Content-Type") != "text/event-stream" {
		var response base.Response
		if err := json.Unmarshal([]byte(answered.body), &response); err != nil {
			t.Fatalf("body is not a response: %v", err)
		}
		if response.Usage == nil {
			t.Fatalf("the answer reports no usage: %s", answered.body)
		}
		return *response.Usage
	}
	payloads := dataPayloads(t, answered.body)
	for index := len(payloads) - 1; index >= 0; index-- {
		var event struct {
			Response *base.Response `json:"response"`
		}
		if err := json.Unmarshal([]byte(payloads[index]), &event); err != nil {
			t.Fatalf("event is not JSON: %v", err)
		}
		if event.Response != nil && event.Response.Usage != nil {
			return *event.Response.Usage
		}
	}
	t.Fatalf("no event in the stream reports usage")
	return base.Usage{}
}

func assertTokenCounts(t *testing.T, record map[string]any, usage base.Usage) {
	t.Helper()
	cached, reasoning := 0, 0
	if usage.InputTokenDetails != nil {
		cached = usage.InputTokenDetails.CachedTokens
	}
	if usage.OutputTokenDetails != nil {
		reasoning = usage.OutputTokenDetails.ReasoningTokens
	}
	counts := map[string]int{
		"input_tokens":     usage.InputTokens,
		"cached_tokens":    cached,
		"output_tokens":    usage.OutputTokens,
		"reasoning_tokens": reasoning,
		"total_tokens":     usage.TotalTokens,
	}
	for key, count := range counts {
		got, counted := record[key]
		if !counted {
			t.Errorf("the record carries no %s: %v", key, record)
			continue
		}
		if got != float64(count) {
			t.Errorf("%s = %v, want %d", key, got, count)
		}
	}
	if usage.OutputTokenDetails == nil {
		t.Error("the fixture reports no reasoning, so the detail counts are untested here")
	}
}

// allowlist resolves the variable the way the command does, so a test asserts
// against the list an operator would actually have configured.
func allowlist(t *testing.T, value string, configured bool) []string {
	t.Helper()
	if !configured {
		noAllowlist(t)
	} else {
		t.Setenv(serve.LogHeaderVar, value)
	}
	names, err := serve.Allowlist()
	if err != nil {
		t.Fatalf("%s=%q: %v", serve.LogHeaderVar, value, err)
	}
	return names
}

// noAllowlist unsets the variable for the duration of a test, so that a value
// in the environment a test happens to run in cannot decide what it asserts.
func noAllowlist(t *testing.T) {
	t.Helper()
	t.Setenv(serve.LogHeaderVar, "")
	if err := os.Unsetenv(serve.LogHeaderVar); err != nil {
		t.Fatalf("unsetting %s: %v", serve.LogHeaderVar, err)
	}
}

func decodeJSON(t *testing.T, label, body string) any {
	t.Helper()
	var decoded any
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("%s body is not JSON: %v", label, err)
	}
	return decoded
}

func findService(rows []serve.Service, name string) (serve.Service, bool) {
	for _, row := range rows {
		if row.Name == name {
			return row, true
		}
	}
	return serve.Service{}, false
}

// fieldNames reads the keys each row actually carries, which is how a field
// nobody meant to publish is caught rather than assumed absent.
func fieldNames(t *testing.T, body string) [][]string {
	t.Helper()
	var rows []map[string]any
	if err := json.Unmarshal([]byte(body), &rows); err != nil {
		t.Fatalf("services is not a table: %v", err)
	}
	names := make([][]string, 0, len(rows))
	for _, row := range rows {
		names = append(names, slices.Sorted(maps.Keys(row)))
	}
	return names
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

func lastBytes(text string) string {
	if len(text) > 120 {
		return text[len(text)-120:]
	}
	return text
}
