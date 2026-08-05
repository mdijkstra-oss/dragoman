package serve

import "encoding/json"

// A caller placing an explicit cache breakpoint is usually too far upstream to know
// which model answers, and support is gated on the model rather than the protocol. So
// a request may declare that its model refuses them, and dragoman removes them on the
// way in — before a dialect sees the request, so no dialect learns of this at all.
//
// prompt_cache_options goes with them: it carries their TTL and means nothing without
// a breakpoint to apply it to. prompt_cache_key stays, because implicit caching is a
// separate mechanism that the models refusing breakpoints still have.
const breakpointsParam = "prompt_cache_breakpoints"

// Only an explicit false disables them. Absent, empty, or anything else leaves the
// request as it arrived, so a caller that has never heard of this is unaffected.
func breakpointsDisabled(value string) bool { return value == "false" }

// The body is rebuilt only where something was removed, so a request carrying no
// breakpoint travels as the exact bytes it arrived in.
func stripCacheBreakpoints(body []byte) []byte {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return body
	}

	input, changed := strippedInput(fields["input"])
	if _, present := fields["prompt_cache_options"]; present {
		delete(fields, "prompt_cache_options")
		changed = true
	}
	if !changed {
		return body
	}
	if input != nil {
		fields["input"] = input
	}

	rebuilt, err := json.Marshal(fields)
	if err != nil {
		return body
	}
	return rebuilt
}

func strippedInput(raw json.RawMessage) (json.RawMessage, bool) {
	var items []json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &items) != nil {
		return nil, false
	}
	changed := false
	for i, item := range items {
		stripped, itemChanged := strippedItem(item)
		if itemChanged {
			items[i] = stripped
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	rebuilt, err := json.Marshal(items)
	if err != nil {
		return nil, false
	}
	return rebuilt, true
}

func strippedItem(raw json.RawMessage) (json.RawMessage, bool) {
	var item map[string]json.RawMessage
	if json.Unmarshal(raw, &item) != nil {
		return raw, false
	}
	content, changed := strippedContent(item["content"])
	if !changed {
		return raw, false
	}
	item["content"] = content
	rebuilt, err := json.Marshal(item)
	if err != nil {
		return raw, false
	}
	return rebuilt, true
}

// Content arrives as a bare string as often as a parts array, and a string carries no
// breakpoint, so failing to decode parts is the ordinary case and not an error.
func strippedContent(raw json.RawMessage) (json.RawMessage, bool) {
	var parts []map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &parts) != nil {
		return raw, false
	}
	changed := false
	for _, part := range parts {
		if _, present := part["prompt_cache_breakpoint"]; present {
			delete(part, "prompt_cache_breakpoint")
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	rebuilt, err := json.Marshal(parts)
	if err != nil {
		return raw, false
	}
	return rebuilt, true
}
