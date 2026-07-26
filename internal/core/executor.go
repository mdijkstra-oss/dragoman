package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/matthijn/dragoman/internal/base"
)

// Call is one outbound request, fully translated.
type Call struct {
	URL     string
	Headers map[string]string
	Body    json.RawMessage
	Stream  bool
}

// Executor performs a call. It stays an interface with one HTTP
// implementation so that routing through something other than HTTP later is
// additive rather than a refactor.
type Executor interface {
	Do(ctx context.Context, call Call) (json.RawMessage, error)
	Stream(ctx context.Context, call Call, onFrame func(base.Frame) error) error
}

// HTTPExecutor is the one implementation. Its timeout has to allow a
// non-streaming request to legitimately run long, because a backend whose
// required output cap had to be guessed is sized not to be the thing that
// ruins the answer.
type HTTPExecutor struct {
	Client *http.Client
}

// NewHTTPExecutor builds an executor with a timeout sized for reasoning models
// answering under a large output cap.
func NewHTTPExecutor() HTTPExecutor {
	return HTTPExecutor{Client: &http.Client{Timeout: 30 * time.Minute}}
}

func (e HTTPExecutor) Do(ctx context.Context, call Call) (json.RawMessage, error) {
	resp, err := e.send(ctx, call)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, Fail(ExitBackend, "reading response: %s", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return body, Fail(ExitBackend, "backend returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return body, nil
}

func (e HTTPExecutor) Stream(ctx context.Context, call Call, onFrame func(base.Frame) error) error {
	resp, err := e.send(ctx, call)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return Fail(ExitBackend, "backend returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return ReadFrames(resp.Body, onFrame)
}

// ReadFrames splits a response stream into frames. It accepts both
// server-sent events and bare newline-delimited JSON, because framing belongs
// to the transport and the dialects disagree about which they use.
func ReadFrames(reader io.Reader, onFrame func(base.Frame) error) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	name := ""
	data := []byte(nil)
	deliver := func() error {
		if len(data) == 0 {
			name = ""
			return nil
		}
		frame := base.Frame{Event: name, Data: data}
		name, data = "", nil
		return onFrame(frame)
	}

	for scanner.Scan() {
		line := scanner.Bytes()
		trimmed := bytes.TrimRight(line, "\r")

		if len(trimmed) == 0 {
			if err := deliver(); err != nil {
				return err
			}
			continue
		}
		if bytes.HasPrefix(trimmed, []byte(":")) {
			continue
		}
		if value, found := cut(trimmed, "event:"); found {
			name = string(bytes.TrimSpace(value))
			continue
		}
		if value, found := cut(trimmed, "data:"); found {
			data = append(data, bytes.TrimSpace(value)...)
			continue
		}
		if trimmed[0] == '{' || trimmed[0] == '[' {
			data = append(data, trimmed...)
			if err := deliver(); err != nil {
				return err
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return Fail(ExitBackend, "reading stream: %s", err)
	}
	return deliver()
}

func (e HTTPExecutor) send(ctx context.Context, call Call) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, call.URL, bytes.NewReader(call.Body))
	if err != nil {
		return nil, Fail(ExitUsage, "building request: %s", err)
	}
	for name, value := range call.Headers {
		request.Header.Set(name, value)
	}
	resp, err := e.Client.Do(request)
	if err != nil {
		return nil, Fail(ExitBackend, "calling %s: %s", call.URL, err)
	}
	return resp, nil
}

func cut(line []byte, prefix string) ([]byte, bool) {
	if !bytes.HasPrefix(line, []byte(prefix)) {
		return nil, false
	}
	return line[len(prefix):], true
}
