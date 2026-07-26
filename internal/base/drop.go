package base

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// DropReason separates the two kinds of drop, because they have opposite
// lifetimes. Without the distinction the backlog is invisible inside a list of
// permanent impossibilities.
type DropReason string

const (
	// DropUnsupported: the target dialect has no equivalent and never will.
	DropUnsupported DropReason = "unsupported"
	// DropUnimplemented: dragoman could map it and does not yet.
	DropUnimplemented DropReason = "unimplemented"
)

// Drop is a field present in a body that no typed field consumed. Entries
// carry a path, not a name, because skip-and-continue makes this the caller's
// only signal.
type Drop struct {
	Path   string     `json:"path"`
	Reason DropReason `json:"reason"`
	Detail string     `json:"detail,omitempty"`
}

// Diff computes the dropped fields of a body against the typed value that
// consumed it. It is computed, never hand-maintained, so an unmapped field is
// loud by default and adding support for it means adding a typed field and
// watching it stop appearing.
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

// Classify assigns each drop its reason from a per-dialect table keyed by the
// leaf field name. The diff cannot tell the two kinds apart; the dialect table
// says which. Anything the table does not name is unimplemented, so the
// backlog stays visible.
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

// LeafName returns the final field name of a drop path, with any array
// subscripts removed.
func LeafName(path string) string {
	if index := strings.LastIndex(path, "."); index >= 0 {
		path = path[index+1:]
	}
	if index := strings.Index(path, "["); index >= 0 {
		path = path[:index]
	}
	return path
}

// FormatDrops renders drops one per line for stderr.
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

// MergeDrops concatenates drop lists, keeping the first entry for each path.
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

// carriesNothing reports whether a value could not have carried an answer. A
// key present on the wire as null, an empty string, an empty array or an empty
// object cannot produce a confident, well-formed, wrong result by going
// missing, and reporting it would bury the drops that can — a block announced
// as {"text": ""} at open is the same block whose text arrives as deltas, and
// an empty input list is consumed rather than lost.
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
