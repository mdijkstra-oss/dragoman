package main

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

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

func TestHandlerStreamingStartsBeforeRunnerCompletes(t *testing.T) {
	runner, records := fakeRunner(t)
	defer runner.Close()
	server := httptest.NewServer(NewHandler(runner, slog.New(slog.NewTextHandler(io.Discard, nil))))
	defer server.Close()

	context, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(context, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"delayed","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("stream did not start before runner completed: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream status/headers: %d %v", response.StatusCode, response.Header)
	}

	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if line != "event: response.created\n" {
		t.Fatalf("first stream event = %q", line)
	}

	if err := os.WriteFile(records+"/release", nil, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHandlerStreamingKeepsDelayedRunnerAlive(t *testing.T) {
	runner, _ := fakeRunner(t)
	defer runner.Close()
	h := handler{
		runner:            runner,
		log:               slog.New(slog.NewTextHandler(io.Discard, nil)),
		heartbeatInterval: time.Millisecond,
	}
	server := httptest.NewServer(http.HandlerFunc(h.responses))
	defer server.Close()

	context, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(context, http.MethodPost, server.URL, strings.NewReader(`{"model":"gpt-5.4","input":"delayed","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()

	reader := bufio.NewReader(response.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended before a keepalive: %v", err)
		}
		if line == ": keepalive\n" {
			return
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
