package serve_test

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/mdijkstra-oss/dragoman/internal/catalog"
	"github.com/mdijkstra-oss/dragoman/internal/core"
	"github.com/mdijkstra-oss/dragoman/internal/transport/pipe"
	"github.com/mdijkstra-oss/dragoman/internal/transport/serve"
)

// TestTranslateOnlyAnswersWithWhatThePipeWouldSend is the claim the mode rests
// on: the body it hands back is the one --no-execute writes to stdout, so a
// caller reading it is reading what the provider would have received and not a
// second rendering of it.
func TestTranslateOnlyAnswersWithWhatThePipeWouldSend(t *testing.T) {
	for _, service := range fixtureServices {
		t.Run(service, func(t *testing.T) {
			body := requestBody(t, false)
			server := listen(t, serve.Config{Executor: fixtures(service), TranslateOnly: true})
			answered := send(t, server, http.MethodPost, responses, addressed(t, body, service))

			if answered.status != http.StatusOK {
				t.Fatalf("status = %d, want 200: %s", answered.status, answered.body)
			}
			if got := answered.header.Get("Content-Type"); got != "application/json" {
				t.Errorf("content type = %q, want application/json", got)
			}
			piped := pipeSent(t, service, body)
			if !reflect.DeepEqual(decodeJSON(t, "listener", answered.body), decodeJSON(t, "pipe", piped)) {
				t.Errorf("listener and --no-execute disagree:\n listener: %s\n pipe:     %s",
					answered.body, piped)
			}
		})
	}
}

// TestTranslateOnlyReachesNoBackend is the guarantee that makes the mode safe
// to point at a production key, and the whole reason for it. A status code
// cannot show it, so the executor is counted instead.
func TestTranslateOnlyReachesNoBackend(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		calls := 0
		server := listen(t, serve.Config{
			Executor:      counted{inner: fixtures("anthropic"), calls: &calls},
			TranslateOnly: true,
		})
		answered := send(t, server, http.MethodPost, responses,
			addressed(t, requestBody(t, streaming), "anthropic"))

		if answered.status != http.StatusOK {
			t.Fatalf("streaming=%v: status = %d, want 200: %s", streaming, answered.status, answered.body)
		}
		if calls != 0 {
			t.Errorf("streaming=%v: the backend was called %d times, want none", streaming, calls)
		}
		if strings.Contains(answered.body, leakSentinel) {
			t.Errorf("streaming=%v: the credential leaked into the answer: %s", streaming, answered.body)
		}
	}
}

// TestTranslateOnlyNeedsNoCredential covers the machine the mode exists for:
// one holding no provider keys at all. Asking for a key there would refuse the
// only request that never spends one.
func TestTranslateOnlyNeedsNoCredential(t *testing.T) {
	server := listen(t, serve.Config{Executor: fixtures("anthropic"), TranslateOnly: true})
	withoutKeys(t)

	answered := send(t, server, http.MethodPost, responses,
		addressed(t, requestBody(t, false), "anthropic"))
	if answered.status != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", answered.status, answered.body)
	}
	if len(answered.body) == 0 {
		t.Error("the translation is empty")
	}
}

// TestServingStillNeedsACredential is the other half of the exemption above: it
// belongs to the mode, and a listener not in it is unchanged.
func TestServingStillNeedsACredential(t *testing.T) {
	server := listen(t, serve.Config{Executor: fixtures("anthropic")})
	withoutKeys(t)

	answered := send(t, server, http.MethodPost, responses,
		addressed(t, requestBody(t, false), "anthropic"))
	if answered.status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", answered.status, answered.body)
	}
	if !strings.Contains(answered.body, catalog.Presets()["anthropic"].KeyEnv) {
		t.Errorf("the refusal does not name the variable that is unset: %s", answered.body)
	}
}

// TestTranslateOnlyReadsTheEnvironment covers the only spelling an operator
// gets. A variable set to something unintended has to leave the mode off: on,
// it answers every caller with a request body none of them asked for.
func TestTranslateOnlyReadsTheEnvironment(t *testing.T) {
	for _, testCase := range []struct {
		value string
		want  bool
	}{
		{value: "1", want: true},
		{value: "true", want: true},
		{value: "TRUE", want: true},
		{value: " yes ", want: true},
		{value: "0", want: false},
		{value: "false", want: false},
		{value: "", want: false},
		{value: "maybe", want: false},
	} {
		t.Run(testCase.value, func(t *testing.T) {
			t.Setenv(serve.TranslateOnlyVar, testCase.value)
			if got := serve.TranslateOnly(); got != testCase.want {
				t.Errorf("TranslateOnly() with %q = %v, want %v", testCase.value, got, testCase.want)
			}
		})
	}
}

// pipeSent is the same request run through the other transport at
// --no-execute, which is the body this mode claims to be equal to. It is handed
// no executor, because at --no-execute there is nothing to execute.
func pipeSent(t *testing.T, service string, body []byte) string {
	t.Helper()
	out, errs := &bytes.Buffer{}, &bytes.Buffer{}
	streams := pipe.Streams{In: bytes.NewReader(body), Out: out, Err: errs}
	code := pipe.Run(context.Background(), catalog.New(),
		core.Options{Preset: service, NoExecute: true}, streams, nil, pipe.Report{})
	if code != int(core.ExitOK) {
		t.Fatalf("pipe exit = %d: %s", code, errs.String())
	}
	return out.String()
}

// withoutKeys empties every provider variable for the duration of a test, so
// that what the listener does with none of them is what the test reads.
func withoutKeys(t *testing.T) {
	t.Helper()
	for _, backend := range catalog.Presets() {
		if backend.KeyEnv == "" {
			continue
		}
		t.Setenv(backend.KeyEnv, "")
		if err := os.Unsetenv(backend.KeyEnv); err != nil {
			t.Fatalf("unsetting %s: %v", backend.KeyEnv, err)
		}
	}
}
