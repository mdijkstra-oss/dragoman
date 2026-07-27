package base

import (
	"encoding/json"
	"fmt"
	"strings"
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
// computed, converted, or costed — but the base defines what each field means,
// and the providers disagree about that. Responses counts reasoning inside
// OutputTokens and names the subset in the details, so a backend that reports
// the two separately is assembled into that shape rather than left meaning
// something else under the same key.
type Usage struct {
	InputTokens        int           `json:"input_tokens"`
	InputTokenDetails  *InputDetails `json:"input_tokens_details,omitempty"`
	OutputTokens       int           `json:"output_tokens"`
	OutputTokenDetails *OutputDetail `json:"output_tokens_details,omitempty"`
	TotalTokens        int           `json:"total_tokens"`
}

// InputDetails names the part of the input the provider did not have to read
// again.
type InputDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// OutputDetail names the part of the output that was reasoning. It is a subset
// of OutputTokens, never a sibling of it.
type OutputDetail struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// CountUsage assembles a usage record in the base's shape. Reasoning is
// counted inside the output, and the total is the two sides added, so a caller
// can do arithmetic on these fields whichever backend answered.
func CountUsage(input, output, reasoning, cached int) *Usage {
	usage := &Usage{
		InputTokens:  input,
		OutputTokens: output,
		TotalTokens:  input + output,
	}
	if cached > 0 {
		usage.InputTokenDetails = &InputDetails{CachedTokens: cached}
	}
	if reasoning > 0 {
		usage.OutputTokenDetails = &OutputDetail{ReasoningTokens: reasoning}
	}
	return usage
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

// FindText returns the answer's prose, with the runs of a multi-item response
// joined. dragoman never renders or concatenates on the way out; this reads a
// response that has already been produced.
func FindText(resp Response) string {
	messages := FilterItems(resp.Output, func(item Item) bool { return item.Type == ItemMessage })
	runs := make([]string, 0, len(messages))
	for _, message := range messages {
		runs = append(runs, GetText(message))
	}
	return strings.Join(runs, "")
}
