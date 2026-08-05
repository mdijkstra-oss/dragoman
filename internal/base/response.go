package base

import (
	"encoding/json"
	"fmt"
	"strings"
)

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

	Raw json.RawMessage `json:"-"`
}

// The claude CLI implements --json-schema as a forced call to a StructuredOutput
// tool, which arrives as a tool_use block and decodes into a function_call item.
// Every client reads the reply out of the output text instead.
//
// Only when the caller offered no tools: with tools on the request a
// function_call may be a genuine one meant for the caller to run, and nothing
// distinguishes the two from outside.
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

// Responses counts reasoning inside OutputTokens and names the subset in the
// details. Providers that report the two separately are assembled into that
// shape.
type Usage struct {
	InputTokens        int           `json:"input_tokens"`
	InputTokenDetails  *InputDetails `json:"input_tokens_details,omitempty"`
	OutputTokens       int           `json:"output_tokens"`
	OutputTokenDetails *OutputDetail `json:"output_tokens_details,omitempty"`
	TotalTokens        int           `json:"total_tokens"`
}

// Without the write count, a cached_tokens of 0 on a first request means either
// the marker did nothing or the cache was just written — opposite outcomes.
type InputDetails struct {
	CachedTokens     int `json:"cached_tokens"`
	CacheWriteTokens int `json:"cache_write_tokens,omitempty"`
}

// Reasoning tokens are a subset of OutputTokens, never a sibling of them.
type OutputDetail struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// A write with nothing read is the first half of a caching exchange and reports
// cached_tokens: 0, so the details exist for either count. Each provider spells
// both somewhere different and one reports no write at all.
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

type IncompleteDetails struct {
	Reason string `json:"reason"`
}

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

func FindText(resp Response) string {
	messages := FilterItems(resp.Output, func(item Item) bool { return item.Type == ItemMessage })
	runs := make([]string, 0, len(messages))
	for _, message := range messages {
		runs = append(runs, GetText(message))
	}
	return strings.Join(runs, "")
}
