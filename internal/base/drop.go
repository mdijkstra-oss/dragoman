package base

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// A detail is what the dialect has to say about the loss. The field diff finds
// the rest, and has nothing to say about a field nothing read.
type Drop struct {
	Path   string `json:"path"`
	Detail string `json:"detail,omitempty"`
}

func Diff(raw json.RawMessage, typed any) ([]Drop, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var source any
	if err := json.Unmarshal(raw, &source); err != nil {
		return nil, fmt.Errorf("diff source: %w", err)
	}
	encoded, err := json.Marshal(typed)
	if err != nil {
		return nil, fmt.Errorf("diff typed: %w", err)
	}
	var consumed any
	if err := json.Unmarshal(encoded, &consumed); err != nil {
		return nil, fmt.Errorf("diff consumed: %w", err)
	}
	drops := walk("", source, consumed, nil)
	sort.SliceStable(drops, func(a, b int) bool { return drops[a].Path < drops[b].Path })
	return drops, nil
}

func FormatDrops(drops []Drop) string {
	lines := make([]string, 0, len(drops))
	for _, drop := range drops {
		line := "dropped: " + drop.Path
		if drop.Detail != "" {
			line += " — " + drop.Detail
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func MergeDrops(groups ...[]Drop) []Drop {
	seen := map[string]bool{}
	merged := []Drop{}
	for _, group := range groups {
		for _, drop := range group {
			if seen[drop.Path] {
				continue
			}
			seen[drop.Path] = true
			merged = append(merged, drop)
		}
	}
	return merged
}

func walk(path string, source, consumed any, drops []Drop) []Drop {
	switch typed := source.(type) {
	case map[string]any:
		other, sameShape := consumed.(map[string]any)
		if !sameShape {
			return drops
		}
		for _, key := range sortedKeys(typed) {
			child := join(path, key)
			value, present := other[key]
			if !present {
				if carriesNothing(typed[key]) {
					continue
				}
				drops = append(drops, Drop{Path: child})
				continue
			}
			drops = walk(child, typed[key], value, drops)
		}
	case []any:
		other, sameShape := consumed.([]any)
		if !sameShape {
			return drops
		}
		for index, element := range typed {
			child := fmt.Sprintf("%s[%d]", path, index)
			if index >= len(other) {
				drops = append(drops, Drop{Path: child})
				continue
			}
			drops = walk(child, element, other[index], drops)
		}
	}
	return drops
}

// A block announced as {"text": ""} at open is the same block whose text
// arrives as deltas, and an empty input list is consumed rather than lost.
func carriesNothing(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return typed == ""
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	}
	return false
}

func sortedKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}
