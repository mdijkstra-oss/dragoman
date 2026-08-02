package dialect

import (
	"fmt"

	"github.com/matthijn/dragoman/internal/base"
)

// Carries declares which base request fields a dialect places somewhere. The
// diff finds keys the base did not model; this says what the dialect can
// express. A field the caller sent that neither covers is a drop, because
// silence is not an option: a dropped field produces a confident, well-formed,
// wrong answer.
type Carries []string

// Handlers is a dispatch table over the four item types the base moves, so
// that walking an input list is written once rather than once per dialect
// (R-2, R-4).
type Handlers struct {
	Message   func(base.Item)
	Reasoning func(base.Item)
	Call      func(base.Item) error
	Output    func(base.Item)
}

// WalkItems dispatches each input item and reports the ones dragoman does not
// move. An item type is the caller's value, not a set dragoman owns, so an
// unrecognised one is reported rather than panicked on.
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
			// The reason is left to the dialect's table, which defaults it to
			// unimplemented. Deciding it here would say every field a dialect
			// does not place is one dragoman could place and has not, and some
			// of them are fields the target has no equivalent for at all.
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

// KnownChoice returns the tool choice a dialect can act on, or nil when the
// caller sent a shape the base does not model. Reporting it beats inventing a
// wire value the provider will reject or, worse, silently ignore.
func KnownChoice(req base.Request) *base.ToolChoice {
	if req.ToolChoice == nil || !req.ToolChoice.Known() {
		return nil
	}
	return req.ToolChoice
}

// suppliedFields lists the base fields the caller actually set, by their wire
// names, so that a dialect's Carries list can be subtracted from it.
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
