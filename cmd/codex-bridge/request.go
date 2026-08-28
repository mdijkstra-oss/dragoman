package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mdijkstra-oss/dragoman/internal/base"
)

const defaultInstructions = "Answer the user request directly. Do not use tools."

type Invocation struct {
	Model        string
	Instructions string
	Effort       string
	Prompt       string
	Schema       json.RawMessage
}

func parseInvocation(body []byte) (Invocation, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return Invocation{}, false, fmt.Errorf("parsing the request: %w", err)
	}
	for name, value := range fields {
		if supportedTopLevel[name] || inert(value) {
			continue
		}
		return Invocation{}, false, fmt.Errorf("codex-bridge: unsupported non-empty field %q", name)
	}

	req, err := base.ParseRequest(body)
	if err != nil {
		return Invocation{}, false, err
	}
	if err := validateInputShape(fields["input"]); err != nil {
		return Invocation{}, false, err
	}
	call := Invocation{Model: base.GetModel(req), Instructions: defaultInstructions}
	if call.Model == "" {
		return Invocation{}, false, fmt.Errorf("codex-bridge: model is required")
	}
	if req.Instructions != nil {
		call.Instructions = *req.Instructions
	}
	if req.Store != nil && *req.Store {
		return Invocation{}, false, fmt.Errorf("codex-bridge: store must be absent or false")
	}
	if len(req.Tools) != 0 {
		return Invocation{}, false, fmt.Errorf("codex-bridge: caller tools are not supported")
	}
	if req.ToolChoice != nil {
		return Invocation{}, false, fmt.Errorf("codex-bridge: tool_choice is not supported")
	}
	if req.MaxOutputTokens != nil {
		return Invocation{}, false, fmt.Errorf("codex-bridge: max_output_tokens has no verified CLI mapping")
	}
	if req.Reasoning != nil {
		if err := onlyKeys(fields["reasoning"], "reasoning", "effort", "summary"); err != nil {
			return Invocation{}, false, err
		}
		if req.Reasoning.Summary != nil {
			return Invocation{}, false, fmt.Errorf("codex-bridge: reasoning summaries are not supported")
		}
		call.Effort = base.GetEffort(req)
	}
	if req.Text != nil {
		if err := onlyKeys(fields["text"], "text", "format"); err != nil {
			return Invocation{}, false, err
		}
		if req.Text.Verbosity != nil {
			return Invocation{}, false, fmt.Errorf("codex-bridge: text.verbosity has no verified CLI mapping")
		}
		format := base.GetFormat(req)
		if format != nil {
			if err := validateFormat(fields["text"], format); err != nil {
				return Invocation{}, false, err
			}
			if format.Type == "json_schema" {
				call.Schema = append(json.RawMessage(nil), format.Schema...)
			}
		}
	}

	prompt, err := oneUserText(req.Input)
	if err != nil {
		return Invocation{}, false, err
	}
	call.Prompt = prompt
	return call, base.GetStream(req), nil
}

var supportedTopLevel = map[string]bool{
	"model": true, "instructions": true, "input": true, "reasoning": true,
	"text": true, "stream": true, "store": true, "tools": true,
	"tool_choice": true, "max_output_tokens": true,
}

func oneUserText(input base.Input) (string, error) {
	if len(input) != 1 {
		return "", fmt.Errorf("codex-bridge: input must contain exactly one user text message")
	}
	item := input[0]
	if item.Type != base.ItemMessage || item.Role != base.RoleUser || len(item.Content) != 1 {
		return "", fmt.Errorf("codex-bridge: input must be exactly one user text message")
	}
	part := item.Content[0]
	if part.Type != base.PartInputText || !inert(part.PromptCacheBreakpoint) {
		return "", fmt.Errorf("codex-bridge: images, cache markers, and non-text input are not supported")
	}
	return part.Text, nil
}

func validateInputShape(raw json.RawMessage) error {
	var prompt string
	if json.Unmarshal(raw, &prompt) == nil {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil || len(items) != 1 {
		return fmt.Errorf("codex-bridge: input must be a string or exactly one user text message")
	}
	var item map[string]json.RawMessage
	if err := json.Unmarshal(items[0], &item); err != nil {
		return fmt.Errorf("codex-bridge: input message must be an object")
	}
	if err := rejectUnknown(item, "input[0]", "type", "role", "content"); err != nil {
		return err
	}
	content := item["content"]
	if json.Unmarshal(content, &prompt) == nil {
		return nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(content, &parts); err != nil || len(parts) != 1 {
		return fmt.Errorf("codex-bridge: input message must contain exactly one text part")
	}
	var part map[string]json.RawMessage
	if err := json.Unmarshal(parts[0], &part); err != nil {
		return fmt.Errorf("codex-bridge: input content must be an object")
	}
	return rejectUnknown(part, "input[0].content[0]", "type", "text", "prompt_cache_breakpoint")
}

func rejectUnknown(object map[string]json.RawMessage, path string, allowed ...string) error {
	for name, value := range object {
		known := false
		for _, candidate := range allowed {
			known = known || name == candidate
		}
		if !known && !inert(value) {
			return fmt.Errorf("codex-bridge: unsupported non-empty field %q", path+"."+name)
		}
	}
	return nil
}

func validateFormat(textRaw json.RawMessage, format *base.Format) error {
	var text map[string]json.RawMessage
	if err := json.Unmarshal(textRaw, &text); err != nil {
		return fmt.Errorf("codex-bridge: text must be an object")
	}
	if err := onlyKeys(text["format"], "text.format", "type", "name", "schema", "strict"); err != nil {
		return err
	}
	switch format.Type {
	case "text":
		var raw map[string]json.RawMessage
		_ = json.Unmarshal(text["format"], &raw)
		for name, value := range raw {
			if name != "type" && !inert(value) {
				return fmt.Errorf("codex-bridge: text format has unsupported field %q", name)
			}
		}
	case "json_schema":
		if len(format.Schema) == 0 || bytes.Equal(format.Schema, []byte("null")) {
			return fmt.Errorf("codex-bridge: text.format.schema is required")
		}
		var schema any
		if json.Unmarshal(format.Schema, &schema) != nil {
			return fmt.Errorf("codex-bridge: text.format.schema is invalid JSON")
		}
	default:
		return fmt.Errorf("codex-bridge: text.format.type %q is not supported", format.Type)
	}
	return nil
}

func onlyKeys(raw json.RawMessage, path string, allowed ...string) error {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return fmt.Errorf("codex-bridge: %s must be an object", path)
	}
	return rejectUnknown(object, path, allowed...)
}

func inert(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("false")) || bytes.Equal(trimmed, []byte(`""`)) {
		return true
	}
	var value any
	if json.Unmarshal(trimmed, &value) != nil {
		return false
	}
	switch value := value.(type) {
	case []any:
		return len(value) == 0
	case map[string]any:
		return len(value) == 0
	}
	return false
}

func tomlString(value string) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func cleanDiagnostic(message string) string { return strings.TrimSpace(message) }
