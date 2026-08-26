package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
)

// The subset of an anthropic-messages request the bridge acts on. Message
// content stays raw so every block dragoman built reaches the CLI byte for byte.
type wireRequest struct {
	Model        string            `json:"model"`
	System       string            `json:"system"`
	Messages     []wireMessage     `json:"messages"`
	Tools        json.RawMessage   `json:"tools"`
	ToolChoice   json.RawMessage   `json:"tool_choice"`
	OutputConfig *wireOutputConfig `json:"output_config"`
	Stream       bool              `json:"stream"`
}

type wireMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type wireOutputConfig struct {
	Effort string `json:"effort"`
	Format *struct {
		Schema json.RawMessage `json:"schema"`
	} `json:"format"`
}

// The type repeats the message's role, which is how the CLI distinguishes a
// turn to run from history to absorb.
type stdinLine struct {
	Type    string      `json:"type"`
	Message wireMessage `json:"message"`
}

type handler struct {
	runner *Runner
	log    *slog.Logger
}

func NewHandler(runner *Runner, log *slog.Logger) http.Handler {
	h := handler{runner: runner, log: log}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", h.messages)
	mux.HandleFunc("POST /messages", h.messages)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"ok":true,"account":%q}`, runner.Account)
	})
	return mux
}

func (h handler) messages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
	if err != nil {
		refuse(w, http.StatusBadRequest, "invalid_request_error", "reading the request: "+err.Error())
		return
	}
	var req wireRequest
	if err := json.Unmarshal(body, &req); err != nil {
		refuse(w, http.StatusBadRequest, "invalid_request_error", "parsing the request: "+err.Error())
		return
	}
	if reason := unservable(req); reason != "" {
		h.log.Warn("refused", "reason", reason)
		refuse(w, http.StatusBadRequest, "invalid_request_error", reason)
		return
	}
	call, err := invocation(req)
	if err != nil {
		refuse(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	flusher, _ := w.(http.Flusher)
	started := false
	send := func(event json.RawMessage) error {
		started = true
		var kind struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(event, &kind)
		h.log.Debug("event", "type", kind.Type)
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", kind.Type, event); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}

	h.log.Info("call", "model", req.Model, "schema", call.Schema != nil, "effort", call.Effort)
	outcome, err := h.runner.Run(r.Context(), call, send)
	switch {
	case err != nil:
		h.log.Error("bridge failed", "error", err.Error())
		if !started {
			refuse(w, http.StatusBadGateway, "api_error", err.Error())
			return
		}
		_ = send(errorEvent("api_error", err.Error()))
	case outcome.Failed:
		message := "the claude CLI reported " + outcome.Status
		if outcome.Message != "" {
			message += ": " + outcome.Message
		}
		h.log.Error("cli failed", "status", outcome.Status, "message", outcome.Message)
		if !started {
			refuse(w, http.StatusBadGateway, "api_error", message)
			return
		}
		_ = send(errorEvent("api_error", message))
	}
}

// Everything the CLI cannot carry is refused rather than dropped, so a caller
// never mistakes a harness answer for a model answer.
func unservable(req wireRequest) string {
	if len(req.Tools) > 0 && string(req.Tools) != "null" && string(req.Tools) != "[]" {
		return "claude-bridge: the claude CLI resolves its own tools and cannot return a tool call to the caller; send no tools"
	}
	if len(req.ToolChoice) > 0 && string(req.ToolChoice) != "null" {
		return "claude-bridge: tool_choice has no tools to choose between"
	}
	userTurns := 0
	for _, message := range req.Messages {
		if message.Role == "user" {
			userTurns++
		}
		if strings.Contains(string(message.Content), `"tool_result"`) || strings.Contains(string(message.Content), `"tool_use"`) {
			return "claude-bridge: a tool exchange in the history cannot be replayed through the claude CLI"
		}
	}
	if userTurns == 0 {
		return "claude-bridge: no user message to answer"
	}
	if userTurns > 1 {
		return fmt.Sprintf("claude-bridge: the claude CLI runs one turn per request; this request has %d user turns", userTurns)
	}
	if !req.Stream {
		return "claude-bridge: the claude CLI answers only as a stream; send stream: true"
	}
	return ""
}

func invocation(req wireRequest) (Invocation, error) {
	call := Invocation{Model: req.Model, System: req.System}
	if config := req.OutputConfig; config != nil {
		call.Effort = config.Effort
		if config.Format != nil && len(config.Format.Schema) > 0 {
			call.Schema = config.Format.Schema
		}
	}
	for _, message := range req.Messages {
		line, err := json.Marshal(stdinLine{Type: message.Role, Message: message})
		if err != nil {
			return Invocation{}, fmt.Errorf("encoding a message: %w", err)
		}
		call.Stdin = append(append(call.Stdin, line...), '\n')
	}
	return call, nil
}

func refuse(w http.ResponseWriter, status int, kind, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": kind, "message": message},
	})
}

func errorEvent(kind, message string) json.RawMessage {
	event, _ := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": kind, "message": message},
	})
	return event
}
