package base

import (
	"encoding/json"
	"fmt"
)

// Response is an openai-responses response body. This is what the caller
// receives no matter which provider answered.
type Response struct {
	ID                string             `json:"id"`
	Object            string             `json:"object"`
	CreatedAt         int64              `json:"created_at"`
	Status            string             `json:"status"`
	Model             string             `json:"model"`
	Output            []Item             `json:"output"`
	Usage             *Usage             `json:"usage,omitempty"`
	IncompleteDetails *IncompleteDetails `json:"incomplete_details,omitempty"`
	Error             *ResponseError     `json:"error,omitempty"`

	// Raw is the backend body exactly as it arrived, kept beside the typed
	// fields so the dropped-field diff can be computed rather than
	// hand-maintained.
	Raw json.RawMessage `json:"-"`
}

// Usage is whatever the backend reported that has an obvious home. Nothing is
// computed, converted, or costed.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// IncompleteDetails names why a response stopped short.
type IncompleteDetails struct {
	Reason string `json:"reason"`
}

// ResponseError is a provider error rendered into the base shape.
type ResponseError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message"`
	Type    string `json:"type,omitempty"`
}

const (
	StatusCompleted  = "completed"
	StatusIncomplete = "incomplete"
	StatusFailed     = "failed"
	StatusInProgress = "in_progress"
)

const (
	ObjectResponse = "response"
)

func ParseResponse(body []byte) (Response, error) {
	var resp Response
	if err := json.Unmarshal(body, &resp); err != nil {
		return Response{}, fmt.Errorf("parse response: %w", err)
	}
	resp.Raw = append(json.RawMessage(nil), body...)
	return resp, nil
}

func FindCalls(resp Response) []Item {
	return FilterItems(resp.Output, func(item Item) bool { return item.Type == ItemFunctionCall })
}
