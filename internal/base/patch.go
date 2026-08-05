package base

import (
	"bytes"
	"encoding/json"
)

// HTML escaping is off because Go's default rewrites <, >, & and the two line
// separators inside every string it encodes, prompt text included.
func Patch(raw json.RawMessage, changes map[string]any) (json.RawMessage, error) {
	if len(changes) == 0 && len(raw) > 0 {
		return raw, nil
	}
	object := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &object); err != nil {
			return nil, err
		}
	}
	for key, value := range changes {
		if value == nil {
			delete(object, key)
			continue
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		object[key] = encoded
	}
	buffer := &bytes.Buffer{}
	encoder := json.NewEncoder(buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(object); err != nil {
		return nil, err
	}
	return json.RawMessage(bytes.TrimRight(buffer.Bytes(), "\n")), nil
}

func Unmodelled(raw json.RawMessage, typed any) ([]Drop, error) { return Diff(raw, typed) }

func Differs(raw json.RawMessage, key string, value any) bool {
	object := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &object); err != nil {
			return true
		}
	}
	current, present := object[key]
	if !present {
		return value != nil
	}
	if value == nil {
		return true
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return true
	}
	return !jsonEqual(current, encoded)
}

func jsonEqual(left, right json.RawMessage) bool {
	var a, b any
	if json.Unmarshal(left, &a) != nil || json.Unmarshal(right, &b) != nil {
		return false
	}
	canonicalA, errA := json.Marshal(a)
	canonicalB, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return string(canonicalA) == string(canonicalB)
}
