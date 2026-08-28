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

type handler struct {
	runner *Runner
	log    *slog.Logger
}

func NewHandler(runner *Runner, log *slog.Logger) http.Handler {
	h := handler{runner: runner, log: log}
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
	result, err := h.runner.Run(request.Context(), call)
	if err != nil {
		h.log.Error("bridge failed", "error", err.Error())
		refuse(w, http.StatusBadGateway, "api_error", err.Error())
		return
	}
	for _, diagnostic := range result.Diagnostics {
		h.log.Warn("codex diagnostic", "message", diagnostic)
	}
	response, events := canonicalResponse(call.Model, result)
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(response); err != nil {
			h.log.Error("writing response", "error", err.Error())
		}
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	for _, event := range events {
		encoded, err := base.EncodeEvent(event)
		if err != nil {
			h.log.Error("encoding event", "error", err.Error())
			return
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.Type, encoded); err != nil {
			return
		}
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func canonicalResponse(model string, result Result) (base.Response, []base.Event) {
	emitter := base.Emitter{
		ResponseID: mintResponseID(),
		Model:      model,
		CreatedAt:  time.Now().Unix(),
	}
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
