package dialect

import (
	"fmt"

	"github.com/mdijkstra-oss/dragoman/internal/base"
)

type Carries []string

type Handlers struct {
	Message   func(base.Item)
	Reasoning func(base.Item)
	Call      func(base.Item) error
	Output    func(base.Item)
}

func WalkItems(items []base.Item, handlers Handlers) ([]base.Drop, error) {
	drops := []base.Drop{}
	for index, item := range items {
		handler, known := map[base.ItemType]func(base.Item) error{
			base.ItemMessage:            wrap(handlers.Message),
			base.ItemReasoning:          wrap(handlers.Reasoning),
			base.ItemFunctionCall:       handlers.Call,
			base.ItemFunctionCallOutput: wrap(handlers.Output),
		}[item.Type]
		if !known || handler == nil {
			drops = append(drops, Unmapped(IndexedPath("input", index), "item type", string(item.Type)))
			continue
		}
		if err := handler(item); err != nil {
			return nil, fmt.Errorf("input[%d]: %w", index, err)
		}
	}
	return drops, nil
}

// FunctionTools splits a tool list into the function tools every dialect
// carries and the drops for the rest. A hosted capability named in the tool
// list belongs to the provider; encoding it as a nameless function would take
// the caller's real tools down with it.
func FunctionTools(tools []base.Tool) ([]base.Tool, []base.Drop) {
	kept := make([]base.Tool, 0, len(tools))
	drops := []base.Drop{}
	for index, tool := range tools {
		if base.IsFunctionTool(tool) {
			kept = append(kept, tool)
			continue
		}
		drops = append(drops, base.Drop{
			Path:   fmt.Sprintf("tools[%d]", index),
			Reason: base.DropUnsupported,
			Detail: "hosted tool " + tool.Type + " is not carried by the base",
		})
	}
	return kept, drops
}

// Unread reports the base fields the caller supplied that a dialect does not
// carry, plus a tool choice whose shape the base does not model.
func Unread(req base.Request, carries Carries) []base.Drop {
	carried := map[string]bool{}
	for _, field := range carries {
		carried[field] = true
	}
	drops := []base.Drop{}
	for _, field := range suppliedFields(req) {
		if !carried[field] {
			drops = append(drops, base.Drop{Path: field})
		}
	}
	if req.ToolChoice != nil && !req.ToolChoice.Known() && carried["tool_choice"] {
		drops = append(drops, base.Drop{
			Path:   "tool_choice",
			Reason: base.DropUnsupported,
			Detail: "only a mode or a named function can be expressed in every dialect",
		})
	}
	return drops
}

// Marked reports whether any of an item's content parts carries a cache
// breakpoint. A dialect that has to spell its parts out to place a marker asks
// this first, because a body with no marker must stay byte-identical to the one
// it sent before breakpoints existed.
func Marked(parts []base.ContentPart) bool {
	for _, part := range parts {
		if part.Marks() {
			return true
		}
	}
	return false
}

// MarkerPath names where in a request a breakpoint was placed. Entries carry a
// path rather than a name, and a marker is only ever reachable at this depth.
func MarkerPath(item, part int) string {
	return fmt.Sprintf("input[%d].content[%d].prompt_cache_breakpoint", item, part)
}

// A wire value dragoman invented here is one the provider rejects or, worse,
// silently ignores.
func KnownChoice(req base.Request) *base.ToolChoice {
	if req.ToolChoice == nil || !req.ToolChoice.Known() {
		return nil
	}
	return req.ToolChoice
}

func suppliedFields(req base.Request) []string {
	supplied := []string{}
	add := func(present bool, name string) {
		if present {
			supplied = append(supplied, name)
		}
	}
	add(req.Model != nil, "model")
	add(req.Instructions != nil, "instructions")
	add(len(req.Input) > 0, "input")
	add(len(req.Tools) > 0, "tools")
	add(req.ToolChoice != nil, "tool_choice")
	add(req.Text != nil && req.Text.Format != nil, "text.format")
	add(req.Text != nil && req.Text.Verbosity != nil, "text.verbosity")
	add(req.Reasoning != nil && req.Reasoning.Effort != nil, "reasoning.effort")
	add(req.Reasoning != nil && req.Reasoning.Summary != nil, "reasoning.summary")
	add(req.MaxOutputTokens != nil, "max_output_tokens")
	add(req.Stream != nil, "stream")
	add(req.ParallelToolCalls != nil, "parallel_tool_calls")
	add(req.Store != nil, "store")
	add(len(req.Include) > 0, "include")
	add(len(req.Metadata) > 0, "metadata")
	add(req.PromptCacheKey != nil, "prompt_cache_key")
	add(req.PromptCacheOptions != nil, "prompt_cache_options")
	return supplied
}

func wrap(handler func(base.Item)) func(base.Item) error {
	if handler == nil {
		return nil
	}
	return func(item base.Item) error {
		handler(item)
		return nil
	}
}
