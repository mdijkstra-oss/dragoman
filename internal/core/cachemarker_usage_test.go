package core_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/mdijkstra-oss/dragoman/internal/base"
	"github.com/mdijkstra-oss/dragoman/internal/catalog"
	"github.com/mdijkstra-oss/dragoman/internal/core"
	"github.com/mdijkstra-oss/dragoman/internal/dialect"
)

// The write count is the instrument the caching feature exists to surface:
// without it a cached_tokens of 0 on a first request means either the marker did
// nothing or the cache was just written, and those are opposite outcomes. Each
// provider spells it somewhere different, so these are the four spellings,
// decoded through the dialects' own extractors and folded by the base.
//
// The counts are the ones observed live against gpt-5.6-luna: run one reports
// {cache_write_tokens: 4190, cached_tokens: 0} and run two reports the pair the
// other way round. Both runs are table rows, because neither alone is the proof.
const (
	cacheWritten = 4190
	cacheRead    = 4190
)

// cachedAnswer is one exchange recorded in the two shapes a provider returns it
// in: the buffered body, and the frames the same answer arrives as when
// streamed. Holding both on one row is what lets the fold invariant be stated
// on the same fixture as the counts.
type cachedAnswer struct {
	name     string
	dialect  string
	buffered string
	frames   []base.Frame
	cached   int
	written  int
}

// cachedAnswers is the matrix: every dialect that reports a write count, on both
// halves of the caching exchange.
func cachedAnswers() []cachedAnswer {
	return []cachedAnswer{
		{
			name:     "openai-responses/write",
			dialect:  "openai-responses",
			buffered: responsesBody(0, cacheWritten),
			frames:   responsesFrames(0, cacheWritten),
			cached:   0,
			written:  cacheWritten,
		},
		{
			name:     "openai-responses/read",
			dialect:  "openai-responses",
			buffered: responsesBody(cacheRead, 0),
			frames:   responsesFrames(cacheRead, 0),
			cached:   cacheRead,
			written:  0,
		},
		{
			name:     "openai-chat-completions/write",
			dialect:  "openai-chat-completions",
			buffered: completionsBody(0, cacheWritten),
			frames:   completionsFrames(0, cacheWritten),
			cached:   0,
			written:  cacheWritten,
		},
		{
			name:     "openai-chat-completions/read",
			dialect:  "openai-chat-completions",
			buffered: completionsBody(cacheRead, 0),
			frames:   completionsFrames(cacheRead, 0),
			cached:   cacheRead,
			written:  0,
		},
		{
			name:     "anthropic-messages/write",
			dialect:  "anthropic-messages",
			buffered: anthropicBody(0, cacheWritten, ""),
			frames:   anthropicFrames(0, cacheWritten, ""),
			cached:   0,
			written:  cacheWritten,
		},
		{
			name:     "anthropic-messages/read",
			dialect:  "anthropic-messages",
			buffered: anthropicBody(cacheRead, 0, ""),
			frames:   anthropicFrames(cacheRead, 0, ""),
			cached:   cacheRead,
			written:  0,
		},
		{
			// This API names only what a read saved. The write count is absent
			// rather than zero-by-accident, which is the same silence a caller
			// gets for marking a request against it at all.
			name:     "gemini-interactions/read",
			dialect:  "gemini-interactions",
			buffered: geminiBody(cacheRead),
			frames:   geminiFrames(cacheRead),
			cached:   cacheRead,
			written:  0,
		},
	}
}

// cachedText is the one sentence of prose every fixture below carries, so that a
// difference between two decoded responses can only be about usage.
const cachedText = "Rio de Janeiro."

// responsesBody is the base's own format, where the write count sits beside the
// read count under input_tokens_details — the spelling the base already used.
func responsesBody(cached, written int) string {
	return `{"id":"resp_c","object":"response","created_at":1785200000,"status":"completed",` +
		`"model":"gpt-5.6-luna","output":[` + responsesItem("completed") + `],` +
		`"usage":` + responsesUsage(cached, written) + `}`
}

func responsesUsage(cached, written int) string {
	return fmt.Sprintf(`{"input_tokens":4290,`+
		`"input_tokens_details":{"cached_tokens":%d,"cache_write_tokens":%d},`+
		`"output_tokens":10,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":4300}`,
		cached, written)
}

func responsesItem(status string) string {
	return `{"id":"msg_c","type":"message","status":"` + status + `","role":"assistant",` +
		`"content":[{"type":"output_text","text":"` + cachedText + `"}]}`
}

func responsesFrames(cached, written int) []base.Frame {
	return dataFrames(
		`{"type":"response.created","sequence_number":1,"response":{"id":"resp_c","object":"response",`+
			`"created_at":1785200000,"status":"in_progress","model":"gpt-5.6-luna","output":[]}}`,
		`{"type":"response.output_item.added","output_index":0,`+
			`"item":{"id":"msg_c","type":"message","status":"in_progress","role":"assistant","content":[]}}`,
		`{"type":"response.output_text.delta","item_id":"msg_c","output_index":0,"content_index":0,`+
			`"delta":"`+cachedText+`"}`,
		`{"type":"response.output_item.done","output_index":0,"item":`+responsesItem("completed")+`}`,
		`{"type":"response.completed","response":`+responsesBody(cached, written)+`}`,
	)
}

// completionsBody is the chat shape, where the same two counts live under
// prompt_tokens_details. The marker that causes them is identical at both
// endpoints serving this shape, and so is the count.
func completionsBody(cached, written int) string {
	return `{"id":"chatcmpl_c","object":"chat.completion","created":1785200000,"model":"gpt-5.6-luna",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"` + cachedText + `"},` +
		`"finish_reason":"stop"}],"usage":` + completionsUsage(cached, written) + `}`
}

func completionsUsage(cached, written int) string {
	return fmt.Sprintf(`{"prompt_tokens":4290,`+
		`"prompt_tokens_details":{"cached_tokens":%d,"cache_write_tokens":%d},`+
		`"completion_tokens":10,"completion_tokens_details":{"reasoning_tokens":0},"total_tokens":4300}`,
		cached, written)
}

// completionsFrames ends with a chunk carrying no choices and only usage. That
// chunk exists because dragoman asked for it with stream_options.include_usage;
// see TestCachedUsageWhileStreamingIsAskedFor.
func completionsFrames(cached, written int) []base.Frame {
	return dataFrames(
		`{"id":"chatcmpl_c","object":"chat.completion.chunk","created":1785200000,"model":"gpt-5.6-luna",`+
			`"choices":[{"index":0,"delta":{"role":"assistant","content":"`+cachedText+`"}}]}`,
		`{"id":"chatcmpl_c","object":"chat.completion.chunk","created":1785200000,"model":"gpt-5.6-luna",`+
			`"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"chatcmpl_c","object":"chat.completion.chunk","created":1785200000,"model":"gpt-5.6-luna",`+
			`"choices":[],"usage":`+completionsUsage(cached, written)+`}`,
	)
}

// anthropicBody spells the write count cache_creation_input_tokens, flat. The
// buckets parameter is the cache_creation object this endpoint also sends; it is
// never read, so the fixtures that carry it prove which field was.
func anthropicBody(cached, written int, buckets string) string {
	return `{"id":"msg_c","type":"message","role":"assistant","model":"claude-sonnet-5",` +
		`"content":[{"type":"text","text":"` + cachedText + `"}],"stop_reason":"end_turn",` +
		`"usage":` + anthropicUsage(cached, written, buckets) + `}`
}

func anthropicUsage(cached, written int, buckets string) string {
	return fmt.Sprintf(`{"input_tokens":100,"output_tokens":10,`+
		`"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d%s,`+
		`"output_tokens_details":{"thinking_tokens":0}}`, cached, written, buckets)
}

// anthropicFrames splits usage the way this endpoint does: the input half
// arrives on message_start and the output half on message_delta, which is why a
// decoder has to carry the first across the whole message.
func anthropicFrames(cached, written int, buckets string) []base.Frame {
	return dataFrames(
		`{"type":"message_start","message":{"id":"msg_c","type":"message","role":"assistant",`+
			`"model":"claude-sonnet-5","content":[],"usage":`+anthropicUsage(cached, written, buckets)+`}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+cachedText+`"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},`+
			`"usage":{"output_tokens":10,"output_tokens_details":{"thinking_tokens":0}}}`,
		`{"type":"message_stop"}`,
	)
}

// geminiBody reports a read and no write at all.
func geminiBody(cached int) string {
	return `{"id":"v1_c","status":"completed","model":"gemini-3.6-flash",` +
		`"created":"2026-07-30T00:00:00Z","steps":[{"type":"model_output",` +
		`"content":[{"type":"text","text":"` + cachedText + `"}]}],` +
		`"usage":` + geminiUsage(cached) + `}`
}

func geminiUsage(cached int) string {
	return fmt.Sprintf(`{"total_tokens":4300,"total_input_tokens":4290,"total_cached_tokens":%d,`+
		`"total_output_tokens":10,"total_thought_tokens":0}`, cached)
}

func geminiFrames(cached int) []base.Frame {
	return dataFrames(
		`{"event_type":"interaction.created","interaction":{"id":"v1_c","status":"in_progress",`+
			`"model":"gemini-3.6-flash","created":"2026-07-30T00:00:00Z"}}`,
		`{"event_type":"step.start","index":0,"step":{"type":"model_output","content":[]}}`,
		`{"event_type":"step.delta","index":0,"delta":{"type":"text","text":"`+cachedText+`"}}`,
		`{"event_type":"step.stop","index":0}`,
		`{"event_type":"interaction.completed","interaction":{"id":"v1_c","status":"completed",`+
			`"model":"gemini-3.6-flash","created":"2026-07-30T00:00:00Z","usage":`+geminiUsage(cached)+`}}`,
	)
}

// foldBody decodes a recorded buffered body through a dialect and folds it, so
// the buffered and the streamed path are compared after exactly the same
// treatment.
func foldBody(t *testing.T, dialectName, body string) base.Response {
	t.Helper()
	spoken, known := catalog.Dialects()[dialectName]
	if !known {
		t.Fatalf("no dialect %q", dialectName)
	}
	events, _, err := dialect.DecodeBody(spoken, []byte(body))
	if err != nil {
		t.Fatalf("%s: decode body: %v", dialectName, err)
	}
	folded, err := base.Fold(events)
	if err != nil {
		t.Fatalf("%s: fold body: %v", dialectName, err)
	}
	return folded
}

func foldFrames(t *testing.T, dialectName string, frames []base.Frame) base.Response {
	t.Helper()
	events, _ := decodeStream(t, dialectName, frames)
	assertEventGrammar(t, events)
	folded, err := base.Fold(events)
	if err != nil {
		t.Fatalf("%s: fold stream: %v", dialectName, err)
	}
	return folded
}

func (a cachedAnswer) fold(t *testing.T, streamed bool) base.Response {
	t.Helper()
	if streamed {
		return foldFrames(t, a.dialect, a.frames)
	}
	return foldBody(t, a.dialect, a.buffered)
}

// cacheDetails insists the details object exists. A first cached request reports
// a write above zero with cached_tokens 0, so details built for the read alone
// would report nothing at all for exactly the exchange the feature exists to
// make visible.
func cacheDetails(t *testing.T, usage *base.Usage) base.InputDetails {
	t.Helper()
	if usage == nil {
		t.Fatal("no usage reported")
	}
	if usage.InputTokenDetails == nil {
		t.Fatal("no input_tokens_details: the cache counts were read off the wire and discarded")
	}
	return *usage.InputTokenDetails
}

func modeName(streamed bool) string {
	if streamed {
		return "streamed"
	}
	return "buffered"
}

// TestCacheWriteCountReachesTheBase is the offline half of the live criterion:
// each provider's own spelling of the two cache counts arrives in the base's
// one spelling, buffered and streamed alike. The pair is the proof — a write
// with nothing read is the first half of an exchange, and a read with nothing
// written is the second — so both runs are asserted rather than one.
func TestCacheWriteCountReachesTheBase(t *testing.T) {
	for _, answer := range cachedAnswers() {
		for _, streamed := range []bool{false, true} {
			t.Run(answer.name+"/"+modeName(streamed), func(t *testing.T) {
				details := cacheDetails(t, answer.fold(t, streamed).Usage)
				if details.CachedTokens != answer.cached {
					t.Errorf("cached_tokens = %d, want %d", details.CachedTokens, answer.cached)
				}
				if details.CacheWriteTokens != answer.written {
					t.Errorf("cache_write_tokens = %d, want %d", details.CacheWriteTokens, answer.written)
				}
			})
		}
	}
}

// TestCacheWriteCountIsAnthropicsFlatTotal covers the one provider that reports
// the same writes twice: a flat total and a split by TTL bucket. The base reads
// the total and not the buckets — one field, one meaning, and no arithmetic.
//
// The buckets here deliberately do not add up to the total. A fixture where they
// agree cannot tell reading the total from summing the buckets, and a test that
// cannot tell is worse than none.
func TestCacheWriteCountIsAnthropicsFlatTotal(t *testing.T) {
	const buckets = `,"cache_creation":{"ephemeral_5m_input_tokens":11,"ephemeral_1h_input_tokens":22}`
	cases := []struct {
		mode string
		fold func(*testing.T) base.Response
	}{
		{
			mode: "buffered",
			fold: func(t *testing.T) base.Response {
				return foldBody(t, "anthropic-messages", anthropicBody(0, cacheWritten, buckets))
			},
		},
		{
			mode: "streamed",
			fold: func(t *testing.T) base.Response {
				return foldFrames(t, "anthropic-messages", anthropicFrames(0, cacheWritten, buckets))
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.mode, func(t *testing.T) {
			details := cacheDetails(t, testCase.fold(t).Usage)
			if details.CacheWriteTokens != cacheWritten {
				t.Errorf("cache_write_tokens = %d, want the flat %d", details.CacheWriteTokens, cacheWritten)
			}
			switch details.CacheWriteTokens {
			case 11 + 22:
				t.Error("the TTL buckets were summed instead of the flat total being read")
			case 11:
				t.Error("one TTL bucket was read instead of the flat total")
			}
		})
	}
}

// TestCachedUsageWhileStreamingIsAskedFor covers why the chat dialect receives a
// usage chunk at all. OpenAI's own chat endpoint reports usage while streaming
// only when asked; every other endpoint serving the same body shape reports it
// unasked, so writing the field in for them would be a body they never sent.
func TestCachedUsageWhileStreamingIsAskedFor(t *testing.T) {
	body := weatherRequest(t)
	streamed, buffered := true, false
	cases := []struct {
		name   string
		preset string
		stream *bool
		want   string
	}{
		{name: "openai-chat asks", preset: "openai-chat", stream: &streamed, want: "true"},
		{name: "openai-chat buffered needs nothing", preset: "openai-chat", stream: &buffered, want: ""},
		{name: "deepseek reports it unasked", preset: "deepseek", stream: &streamed, want: ""},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			sent, _ := translate(t, core.Options{Preset: testCase.preset, Stream: testCase.stream}, body)
			if got := lookup(sent, "stream_options.include_usage"); got != testCase.want {
				t.Errorf("stream_options.include_usage = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestDeepSeekCacheReadIsUnchangedAndItsPairIsSilent covers the endpoint the
// feature buys nothing at: the read count still decodes exactly as it did, no
// write count is invented out of the hit/miss pair beside it, and that pair is
// dropped without a word — response-side drops never diff usage, which is the
// known limit rather than a bug report.
func TestDeepSeekCacheReadIsUnchangedAndItsPairIsSilent(t *testing.T) {
	// The recorded response has to still carry the pair, or the assertions below
	// pass by testing nothing.
	recorded, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "deepseek", "buffered.json"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	for _, spelling := range []string{`"prompt_cache_hit_tokens":256`, `"prompt_cache_miss_tokens":73`, `"cached_tokens":256`} {
		if !strings.Contains(string(recorded), spelling) {
			t.Fatalf("fixture no longer carries %s; recapture it before trusting this test", spelling)
		}
	}

	for _, streamed := range []bool{false, true} {
		t.Run(modeName(streamed), func(t *testing.T) {
			usage := decode(t, "deepseek", streamed).Usage
			details := cacheDetails(t, usage)
			if details.CachedTokens != 256 {
				t.Errorf("cached_tokens = %d, want the 256 the fixture reports", details.CachedTokens)
			}
			if details.CacheWriteTokens != 0 {
				t.Errorf("cache_write_tokens = %d, want 0: this endpoint reports no write count, "+
					"and the miss count is not one", details.CacheWriteTokens)
			}
			// A count of zero is not reported as zero, so no recorded response
			// grows a field the provider never sent.
			if rendered := mustJSON(t, usage); strings.Contains(rendered, "cache_write_tokens") {
				t.Errorf("usage names a write count the provider did not send: %s", rendered)
			}
		})
	}

	credentialFor(t, "deepseek")
	off := false
	result, err := core.Run(context.Background(), catalog.New(),
		core.Options{Preset: "deepseek", Stream: &off}, weatherRequest(t), fixture("deepseek"), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if mentions(result.Drops, "prompt_cache") || mentions(result.Drops, "usage") {
		t.Errorf("the hit/miss pair was reported after all; drops = %+v", result.Drops)
	}
}

// TestGeminiCacheReadHasNoWriteCount covers the provider with a read count and
// nothing to write it with. The read still arrives; the write is absent rather
// than reported as zero, because absent stays absent.
func TestGeminiCacheReadHasNoWriteCount(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		t.Run("marked/"+modeName(streamed), func(t *testing.T) {
			answer := cachedAnswer{
				dialect:  "gemini-interactions",
				buffered: geminiBody(cacheRead),
				frames:   geminiFrames(cacheRead),
			}
			resp := answer.fold(t, streamed)
			details := cacheDetails(t, resp.Usage)
			if details.CachedTokens != cacheRead {
				t.Errorf("cached_tokens = %d, want %d", details.CachedTokens, cacheRead)
			}
			if details.CacheWriteTokens != 0 {
				t.Errorf("cache_write_tokens = %d, want 0: this API reports none", details.CacheWriteTokens)
			}
			if rendered := mustJSON(t, resp.Usage); strings.Contains(rendered, "cache_write_tokens") {
				t.Errorf("a write count was invented: %s", rendered)
			}
		})
	}

	// The recorded call reports nothing cached, so the details object is absent
	// altogether — a pre-feature response must not grow one.
	for _, streamed := range []bool{false, true} {
		t.Run("recorded/"+modeName(streamed), func(t *testing.T) {
			usage := decode(t, "gemini", streamed).Usage
			if usage == nil {
				t.Fatal("no usage reported")
			}
			if usage.InputTokenDetails != nil {
				t.Errorf("input_tokens_details = %+v, want absent when nothing was cached or written",
					*usage.InputTokenDetails)
			}
			if rendered := mustJSON(t, usage); strings.Contains(rendered, "cache") {
				t.Errorf("usage grew a cache field the provider never sent: %s", rendered)
			}
		})
	}
}

// TestFoldEqualsBufferedForCacheUsage states the invariant on the counts the
// feature adds. The two paths read the write count from different frames on two
// of these dialects, so agreeing on the answer while disagreeing on the usage is
// exactly the failure this catches.
func TestFoldEqualsBufferedForCacheUsage(t *testing.T) {
	answers := append(cachedAnswers(), cachedAnswer{
		name:     "anthropic-messages/buckets",
		dialect:  "anthropic-messages",
		buffered: anthropicBody(0, cacheWritten, `,"cache_creation":{"ephemeral_5m_input_tokens":11}`),
		frames:   anthropicFrames(0, cacheWritten, `,"cache_creation":{"ephemeral_5m_input_tokens":11}`),
	})
	for _, answer := range answers {
		t.Run(answer.name, func(t *testing.T) {
			buffered := answer.fold(t, false)
			streamed := answer.fold(t, true)
			if !reflect.DeepEqual(buffered.Usage, streamed.Usage) {
				t.Errorf("usage differs between the two paths\nbuffered: %s\nstreamed: %s",
					mustJSON(t, buffered.Usage), mustJSON(t, streamed.Usage))
			}
			if !reflect.DeepEqual(buffered, streamed) {
				t.Errorf("folding the stream did not equal the buffered response\nbuffered: %+v\nstreamed: %+v",
					buffered, streamed)
			}
		})
	}
}

// TestCacheCountsSurviveTheWholePipeline runs the same recorded exchange through
// core.Run rather than through a decoder directly, so the counts are checked
// where a caller actually reads them: on the response the pipeline returns.
func TestCacheCountsSurviveTheWholePipeline(t *testing.T) {
	credentialFor(t, "openai")
	off := false
	result, err := core.Run(context.Background(), catalog.New(),
		core.Options{Preset: "openai", Stream: &off}, weatherRequest(t),
		replying(responsesBody(0, cacheWritten)), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if result.Response == nil {
		t.Fatal("buffered run returned no response")
	}
	details := cacheDetails(t, result.Response.Usage)
	if details.CacheWriteTokens != cacheWritten {
		t.Errorf("cache_write_tokens = %d, want %d", details.CacheWriteTokens, cacheWritten)
	}
	// And the caller reads it under OpenAI's own name, because the base is that
	// format rather than a format modelled on it.
	var decoded map[string]any
	if err := json.Unmarshal([]byte(mustJSON(t, result.Response)), &decoded); err != nil {
		t.Fatalf("response: %v", err)
	}
	if got := lookup(decoded, "usage.input_tokens_details.cache_write_tokens"); got != "4190" {
		t.Errorf("usage.input_tokens_details.cache_write_tokens = %q, want %q", got, "4190")
	}
}
