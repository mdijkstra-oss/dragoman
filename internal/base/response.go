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

// AsFormattedText moves an answer the backend delivered as a tool call into the
// output text the caller's format asked for. A caller that sets text.format asked
// for a shape, not for a tool, and every client reads the reply out of the output
// text — so a backend that reaches the shape by calling a tool of its own has
// answered the question in a place nobody is listening.
//
// The claude CLI is the backend that does this: --json-schema is implemented as a
// forced call to a StructuredOutput tool, which arrives as a tool_use block and
// decodes into a function_call item.
//
// It applies only when the caller offered no tools. With tools on the request a
// function_call may be a genuine one meant for the caller to run, and no rule
// distinguishes the two from outside; with none, a tool call can only be the
// backend's own machinery for the format.
func AsFormattedText(resp Response, req Request) Response {
	answering, isAnswered := findFormatAnswer(resp, req)
	if !isAnswered {
		return resp
	}
	converted := resp
	converted.Output = make([]Item, 0, len(resp.Output))
	for index, item := range resp.Output {
		if index == answering {
			item = Item{
				Type:    ItemMessage,
				ID:      item.ID,
				Status:  item.Status,
				Role:    RoleAssistant,
				Content: []ContentPart{{Type: PartOutputText, Text: item.Arguments}},
			}
		}
		converted.Output = append(converted.Output, item)
	}
	return converted
}

// findFormatAnswer reports which item carries the answer to the caller's format.
//
// It is the last tool call rather than the first, and rather than all of them. The
// claude CLI checks its own structured answer against the schema and resamples one
// that fails, leaving every attempt in the stream — a rejected draft arrives as a
// tool call carrying unsubstituted $PARAMETER_NAME placeholders, followed by the
// one that survived. Converting them all would concatenate a draft and an answer
// into two JSON values in one output text, which parses as neither.
//
// The drafts are left as tool calls rather than dropped: they carry no output text,
// so nothing reading the reply sees them, and discarding what a backend sent is not
// this function's business.
func findFormatAnswer(resp Response, req Request) (int, bool) {
	format := GetFormat(req)
	if format == nil || format.Type != "json_schema" || len(req.Tools) > 0 {
		return 0, false
	}
	answering, isFound := 0, false
	for index, item := range resp.Output {
		if item.Type == ItemFunctionCall && item.Arguments != "" {
			answering, isFound = index, true
		}
	}
	return answering, isFound
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
// again, and the part it had to write into the cache to get there. The write
// count is the instrument: without it a cached_tokens of 0 on a first request
// means either the marker did nothing or the cache was just written, and those
// are opposite outcomes. Costed nowhere — it is read off the wire and reported.
type InputDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// OutputDetail names the part of the output that was reasoning. It is a subset
// of OutputTokens, never a sibling of it.
type OutputDetail struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// CountUsage assembles a usage record in the base's shape. Reasoning is
// counted inside the output, and the total is the two sides added, so a caller
// can do arithmetic on these fields whichever backend answered.
//
// A write with nothing read is the first half of a caching exchange and reports
// cached_tokens: 0, so the details have to exist for either count rather than
// for the read alone. Each provider spells both somewhere different and one
// reports no write at all; whichever arrived lands here.
func CountUsage(input, output, reasoning, cached, written int) *Usage {
	usage := &Usage{
		InputTokens:  input,
		OutputTokens: output,
		TotalTokens:  input + output,
	}
	if cached > 0 || written > 0 {
		usage.InputTokenDetails = &InputDetails{CachedTokens: cached, CacheWriteTokens: written}
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
