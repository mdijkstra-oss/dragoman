package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/mdijkstra-oss/dragoman/internal/base"
)

func TestHandlerBufferedAndSSEFoldToSameResult(t *testing.T) {
	runner, _ := fakeRunner(t)
	defer runner.Close()
	handler := NewHandler(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))

	buffered := post(t, handler, `{"model":"gpt-5.4","input":"hello"}`)
	if buffered.Code != http.StatusOK {
		t.Fatalf("buffered status %d: %s", buffered.Code, buffered.Body.String())
	}
	var bufferedResponse base.Response
	if err := json.Unmarshal(buffered.Body.Bytes(), &bufferedResponse); err != nil {
		t.Fatal(err)
	}

	streamed := post(t, handler, `{"model":"gpt-5.4","input":"hello","stream":true}`)
	if streamed.Code != http.StatusOK || streamed.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream status/headers: %d %v", streamed.Code, streamed.Header())
	}
	var terminal base.Event
	deltas := 0
	for _, line := range strings.Split(streamed.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event base.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		if event.Type == base.EventTextDelta {
			deltas++
		}
		if event.Type == base.EventCompleted {
			terminal = event
		}
	}
	if deltas != 1 || terminal.Response == nil {
		t.Fatalf("delta count %d, terminal %+v", deltas, terminal)
	}
	if base.FindText(bufferedResponse) != base.FindText(*terminal.Response) || bufferedResponse.Usage.TotalTokens != terminal.Response.Usage.TotalTokens {
		t.Fatalf("buffered and SSE differ:\n%+v\n%+v", bufferedResponse, terminal.Response)
	}
}

func TestHandlerReturns400And502(t *testing.T) {
	runner, _ := fakeRunner(t)
	defer runner.Close()
	handler := NewHandler(runner, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if response := post(t, handler, `{"model":"gpt-5.4","input":"hello","tools":[{"name":"f"}]}`); response.Code != http.StatusBadRequest {
		t.Fatalf("validation status = %d", response.Code)
	}
	for _, prompt := range []string{"malformed", "fail", "exit"} {
		response := post(t, handler, `{"model":"gpt-5.4","input":`+strconv.Quote(prompt)+`}`)
		if response.Code != http.StatusBadGateway {
			t.Fatalf("%s status = %d: %s", prompt, response.Code, response.Body.String())
		}
	}
}

func post(t *testing.T, handler http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
