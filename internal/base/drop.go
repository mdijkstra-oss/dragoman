package base

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type DropReason string

const (
	DropUnsupported   DropReason = "unsupported"
	DropUnimplemented DropReason = "unimplemented"
	DropInert         DropReason = "inert"
)

type Drop struct {
	Path   string     `json:"path"`
	Reason DropReason `json:"reason"`
	Detail string     `json:"detail,omitempty"`
}

func (d Drop) Notable() bool { return d.Reason != DropInert }

func PartitionDrops(drops []Drop) (notable, inert []Drop) {
	for _, drop := range drops {
		if drop.Notable() {
			notable = append(notable, drop)
			continue
		}
		inert = append(inert, drop)
	}
	return notable, inert
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

func Classify(drops []Drop, table map[string]DropReason) []Drop {
	classified := make([]Drop, 0, len(drops))
	for _, drop := range drops {
		if drop.Reason == "" {
			drop.Reason = DropUnimplemented
			if reason, named := table[LeafName(drop.Path)]; named {
				drop.Reason = reason
			}
		}
		classified = append(classified, drop)
	}
	return classified
}

func LeafName(path string) string {
	if index := strings.LastIndex(path, "."); index >= 0 {
		path = path[index+1:]
	}
	if index := strings.Index(path, "["); index >= 0 {
		path = path[:index]
	}
	return path
}

func FormatDrops(drops []Drop) string {
	lines := make([]string, 0, len(drops))
	for _, drop := range drops {
		line := fmt.Sprintf("dropped (%s): %s", drop.Reason, drop.Path)
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
