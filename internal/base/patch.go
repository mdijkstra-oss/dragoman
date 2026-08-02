package base

import (
	"bytes"
	"encoding/json"
)

// Patch applies a set of top-level changes to a body, returning it byte for
// byte unchanged when there is nothing to change. A change decodes the body one
// key at a time into bytes rather than into the typed request, so what it does
// not name survives it: nested values arrive as the caller wrote them, a
// top-level key the base does not model is carried across, and the top-level
// key order is the only thing a change costs. That is what makes the
// openai-responses path equivalent rather than merely close.
//
// The re-encode runs with HTML escaping off, which is what makes that claim
// hold at the byte level: the default rewrites <, >, & and the two line
// separators inside every string it passes, prompt text included, so a body
// whose model was rewritten would reach the backend with its content edited
// too.
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

// Unmodelled lists the fields of a body that the typed base did not consume.
// An encoder that forwards the body verbatim drops none of them; one that
// builds a fresh body from typed fields drops all of them.
func Unmodelled(raw json.RawMessage, typed any) ([]Drop, error) { return Diff(raw, typed) }

// Differs reports whether a body's top-level key already holds a value.
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
