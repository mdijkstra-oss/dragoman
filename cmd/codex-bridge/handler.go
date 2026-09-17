package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/mdijkstra-oss/dragoman/internal/base"
)

const streamHeartbeatInterval = 25 * time.Second

type handler struct {
	runner            *Runner
	log               *slog.Logger
	heartbeatInterval time.Duration
}

func NewHandler(runner *Runner, log *slog.Logger) http.Handler {
	h := handler{runner: runner, log: log, heartbeatInterval: streamHeartbeatInterval}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /responses", h.responses)
	mux.HandleFunc("POST /v1/responses", h.responses)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true,"account":"ChatGPT"}`)
	})
	return mux
}

func (h handler) responses(w http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, request.Body, 64<<20))
	if err != nil {
		refuse(w, http.StatusBadRequest, "invalid_request_error", "reading the request: "+err.Error())
		return
	}
	call, stream, err := parseInvocation(body)
	if err != nil {
		h.log.Warn("refused", "reason", err.Error())
		refuse(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	h.log.Info("call", "model", call.Model, "schema", len(call.Schema) > 0, "effort", call.Effort, "stream", stream)
	if stream {
		h.stream(w, request, call)
		return
	}
	result, err := h.runner.Run(request.Context(), call)
	if err != nil {
		h.log.Error("bridge failed", "error", err.Error())
		refuse(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	for _, diagnostic := range result.Diagnostics {
		h.log.Warn("codex diagnostic", "message", diagnostic)
	}
	response, _ := canonicalResponse(call.Model, result)
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(response); err != nil {
		h.log.Error("writing response", "error", err.Error())
	}
}

func (h handler) stream(w http.ResponseWriter, request *http.Request, call Invocation) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	emitter := responseEmitter(call.Model)
	var started []base.Event
	emitter, started = base.Begin(emitter)
	if err := writeEvents(w, started); err != nil {
		return
	}

	type completedRun struct {
		result Result
		err    error
	}
	completed := make(chan completedRun, 1)
	go func() {
		result, err := h.runner.Run(request.Context(), call)
		completed <- completedRun{result: result, err: err}
	}()

	heartbeats := time.NewTicker(h.heartbeatInterval)
	defer heartbeats.Stop()
	var run completedRun

awaitRun:
	for {
		select {
		case run = <-completed:
			break awaitRun
		case <-heartbeats.C:
			if err := writeKeepalive(w); err != nil {
				return
			}
		}
	}

	if run.err != nil {
		h.log.Error("bridge failed", "error", run.err.Error())
		emitter = base.SetOutcome(emitter, base.StatusFailed, nil, &base.ResponseError{
			Type:    "api_error",
			Message: run.err.Error(),
		})
		_, events := base.Finish(emitter)
		_ = writeEvents(w, events)
		return
	}
	for _, diagnostic := range run.result.Diagnostics {
		h.log.Warn("codex diagnostic", "message", diagnostic)
	}
	_, events := canonicalResponseFromEmitter(emitter, run.result)
	_ = writeEvents(w, events)
}

func writeKeepalive(w http.ResponseWriter) error {
	if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
		return err
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func writeEvents(w http.ResponseWriter, events []base.Event) error {
	for _, event := range events {
		encoded, err := base.EncodeEvent(event)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, encoded); err != nil {
			return err
		}
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func canonicalResponse(model string, result Result) (base.Response, []base.Event) {
	return canonicalResponseFromEmitter(responseEmitter(model), result)
}

func responseEmitter(model string) base.Emitter {
	return base.Emitter{
		ResponseID: mintResponseID(),
		Model:      model,
		CreatedAt:  time.Now().Unix(),
	}
}

func canonicalResponseFromEmitter(emitter base.Emitter, result Result) (base.Response, []base.Event) {
	emitter, events := base.TextDelta(emitter, "answer", result.Answer)
	var closed []base.Event
	emitter, closed = base.Close(emitter, "answer")
	events = append(events, closed...)
	emitter = base.SetUsage(emitter, result.Usage)
	var finished []base.Event
	emitter, finished = base.Finish(emitter)
	events = append(events, finished...)
	return base.Snapshot(emitter, base.StatusCompleted), events
}

func mintResponseID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return fmt.Sprintf("resp_%d", time.Now().UnixNano())
	}
	return "resp_" + hex.EncodeToString(value)
}

func refuse(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": kind, "message": message},
	})
}
