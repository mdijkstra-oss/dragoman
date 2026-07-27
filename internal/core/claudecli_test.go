package core_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/matthijn/dragoman/internal/base"
	"github.com/matthijn/dragoman/internal/catalog"
	"github.com/matthijn/dragoman/internal/core"
)

// spawned is a real executor backed by frames the installed CLI actually
// emitted, so the unwrapping is exercised against bytes the CLI sent rather
// than against bytes a test author imagined.
type spawned struct{ file string }

func (s spawned) Do(_ context.Context, _ core.Call) (json.RawMessage, error) {
	return nil, core.Fail(core.ExitUsage, "the recorded CLI answers only as a stream")
}

func (s spawned) Stream(_ context.Context, _ core.Call, onFrame func(base.Frame) error) error {
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "claude-cli", s.file))
	if err != nil {
		return err
	}
	outcome, err := core.ReadCLIFrames(bytes.NewReader(body), onFrame)
	if err != nil {
		return err
	}
	if outcome.Failed {
		return core.Fail(core.ExitBackend, "%s: %s", outcome.Status, outcome.Message)
	}
	return nil
}

func knowledgeRequest(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "knowledge.json"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return body
}

func recordedFrames(t *testing.T) []byte {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "fixtures", "claude-cli", "stream.ndjson"))
	if err != nil {
		t.Fatalf("fixture: %v", err)
	}
	return body
}

// TestCommandBuildsTheInvocation covers the whole mapping from an encoded body
// onto an invocation. It is pure, so the argv a request produces is checked
// without spawning anything.
func TestCommandBuildsTheInvocation(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		wants  []string
		absent []string
	}{
		{
			name:  "model and system prompt",
			body:  `{"model":"sonnet","system":"be terse","messages":[]}`,
			wants: []string{"--model", "sonnet", "--system-prompt", "be terse"},
		},
		{
			// Omitting it leaves Claude Code's own agent prompt in place, which
			// is a body of meaning the caller never asked for.
			name:   "system prompt is passed even when empty",
			body:   `{"model":"sonnet","messages":[]}`,
			wants:  []string{"--system-prompt", ""},
			absent: []string{"--effort", "--json-schema"},
		},
		{
			name:   "absent model stays absent",
			body:   `{"messages":[]}`,
			absent: []string{"--model"},
		},
		{
			name:  "effort passes through untouched",
			body:  `{"model":"m","output_config":{"effort":"XiGhLy-Bespoke"},"messages":[]}`,
			wants: []string{"--effort", "XiGhLy-Bespoke"},
		},
		{
			name:  "schema reaches the structured-output flag",
			body:  `{"model":"m","output_config":{"format":{"type":"json_schema","schema":{"type":"object"}}},"messages":[]}`,
			wants: []string{"--json-schema", `{"type":"object"}`},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			argv, _, err := core.Command([]byte(testCase.body))
			if err != nil {
				t.Fatalf("command: %v", err)
			}
			for index := 0; index < len(testCase.wants); index++ {
				if !contains(argv, testCase.wants[index]) {
					t.Errorf("argv is missing %q: %q", testCase.wants[index], argv)
				}
			}
			for _, flag := range testCase.absent {
				if contains(argv, flag) {
					t.Errorf("argv carries %q, want absent: %q", flag, argv)
				}
			}
		})
	}
}

// TestOpacityFlagsAreExact pins the invocation that turns an agent back into a
// model endpoint. Names alone are not enough: `--tools ""` and `--tools all`
// are the same flag and opposite behaviours, and dropping the empty value would
// silently make the next flag its argument. So the whole prefix is asserted in
// order, and any change to it has to be a deliberate edit here.
func TestOpacityFlagsAreExact(t *testing.T) {
	want := []string{
		"--print",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"--safe-mode",
		"--tools", "",
		"--disable-slash-commands",
		"--strict-mcp-config",
		"--no-session-persistence",
		"--system-prompt", "",
	}
	argv, _, err := core.Command([]byte(`{"messages":[]}`))
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	if !slices.Equal(argv, want) {
		t.Errorf("invocation drifted\n got: %q\nwant: %q", argv, want)
	}
}

// TestCommandWritesOneLinePerMessage covers the stdin half. The CLI reads a
// history as newline-delimited messages rather than as an array, and content
// passes through as the bytes the dialect built.
func TestCommandWritesOneLinePerMessage(t *testing.T) {
	body := `{"model":"m","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"first"}]},` +
		`{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"sig-abc"}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"ok"}]}]}`

	_, stdin, err := core.Command([]byte(body))
	if err != nil {
		t.Fatalf("command: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(stdin), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines = %d, want 3: %s", len(lines), stdin)
	}

	types := []string{}
	for _, line := range lines {
		var framed struct {
			Type    string `json:"type"`
			Message struct {
				Role string `json:"role"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(line), &framed); err != nil {
			t.Fatalf("line is not JSON: %v", err)
		}
		if framed.Type != framed.Message.Role {
			t.Errorf("type = %q, want the role %q", framed.Type, framed.Message.Role)
		}
		types = append(types, framed.Type)
	}
	if strings.Join(types, ",") != "user,assistant,user" {
		t.Errorf("types = %v, want user,assistant,user", types)
	}
	// Provider state is what a turn boundary is most likely to lose, and the
	// signature has to arrive byte-identical or the backend rejects it.
	if !strings.Contains(lines[1], `"signature":"sig-abc"`) {
		t.Errorf("the signature did not survive: %s", lines[1])
	}
	if !strings.Contains(lines[2], `"tool_use_id":"call_1"`) {
		t.Errorf("the call identity did not survive: %s", lines[2])
	}
}

// TestBinaryOfReadsTheEndpoint covers the scheme that names a process rather
// than a URL.
func TestBinaryOfReadsTheEndpoint(t *testing.T) {
	cases := []struct {
		endpoint string
		want     string
		fails    bool
	}{
		{endpoint: "exec:claude", want: "claude"},
		{endpoint: "exec:/opt/bin/claude", want: "/opt/bin/claude"},
		{endpoint: "exec:", fails: true},
		{endpoint: "claude", fails: true},
		{endpoint: "https://api.anthropic.com/v1", fails: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.endpoint, func(t *testing.T) {
			got, err := core.BinaryOf(testCase.endpoint)
			if testCase.fails {
				if err == nil {
					t.Fatalf("got %q, want a failure", got)
				}
				if core.CodeOf(err) != core.ExitUsage {
					t.Errorf("exit = %d, want %d", core.CodeOf(err), core.ExitUsage)
				}
				return
			}
			if err != nil {
				t.Fatalf("binary: %v", err)
			}
			if got != testCase.want {
				t.Errorf("binary = %q, want %q", got, testCase.want)
			}
		})
	}
}

// TestReadCLIFramesKeepsOnlyModelOutput covers the unwrapping. The CLI's own
// bookkeeping is not an answer, and an unrecognised frame must not break a
// translation, because the CLI adds frame types between releases.
func TestReadCLIFramesKeepsOnlyModelOutput(t *testing.T) {
	frames := []base.Frame{}
	outcome, err := core.ReadCLIFrames(bytes.NewReader(recordedFrames(t)), func(frame base.Frame) error {
		frames = append(frames, frame)
		return nil
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !outcome.Seen {
		t.Error("the terminal result frame was not read")
	}
	if outcome.Failed {
		t.Errorf("a successful recording reported failure: %+v", outcome)
	}
	if len(frames) == 0 {
		t.Fatal("no dialect events survived the unwrapping")
	}
	// What comes out is anthropic-messages, not the CLI's envelope: a frame
	// still carrying session bookkeeping would mean the wrapper leaked.
	for _, frame := range frames {
		var event struct {
			Type      string `json:"type"`
			SessionID string `json:"session_id"`
		}
		if err := json.Unmarshal(frame.Data, &event); err != nil {
			t.Fatalf("unwrapped frame is not JSON: %v", err)
		}
		if event.SessionID != "" {
			t.Errorf("the CLI envelope leaked into a dialect event: %s", frame.Data)
		}
		if !strings.HasPrefix(event.Type, "message_") && !strings.HasPrefix(event.Type, "content_block_") {
			t.Errorf("frame %q is not an anthropic-messages event", event.Type)
		}
	}
}

// TestCLIFrameTypesAreFilteredNotFatal covers the frames the CLI emits that are
// not model output, including one no release has emitted yet.
func TestCLIFrameTypesAreFilteredNotFatal(t *testing.T) {
	lines := strings.Join([]string{
		`{"type":"system","subtype":"init","cwd":"/workspace"}`,
		`{"type":"a_frame_type_from_a_later_release","payload":{"deeply":{"nested":true}}}`,
		`not json at all`,
		`{"type":"stream_event","event":{"type":"message_stop"},"session_id":"s"}`,
		`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed"}}`,
		`{"is_error":false,"result":"done","type":"result"}`,
	}, "\n")

	kept := []string{}
	outcome, err := core.ReadCLIFrames(strings.NewReader(lines), func(frame base.Frame) error {
		kept = append(kept, string(frame.Data))
		return nil
	})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(kept) != 1 || !strings.Contains(kept[0], "message_stop") {
		t.Errorf("kept = %v, want only the dialect event", kept)
	}
	if !outcome.Seen || outcome.Failed {
		t.Errorf("outcome = %+v, want a successful result", outcome)
	}
}

// TestCLIErrorFrameIsABackendFailure covers failure arriving in the stream
// rather than in an exit status. A CI step gating on the exit code has to see
// it as the backend saying no, which is one, not two.
func TestCLIErrorFrameIsABackendFailure(t *testing.T) {
	const failed = `{"type":"stream_event","event":{"type":"message_start","message":{"id":"m","model":"x"}},"session_id":"s"}
{"is_error":true,"api_error_status":404,"result":"There's an issue with the selected model.","type":"result"}`

	outcome, err := core.ReadCLIFrames(strings.NewReader(failed), func(base.Frame) error { return nil })
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !outcome.Failed {
		t.Fatal("an error frame was read as success")
	}
	if !strings.Contains(outcome.Status, "404") {
		t.Errorf("status = %q, want the reported HTTP status", outcome.Status)
	}
	if !strings.Contains(outcome.Message, "selected model") {
		t.Errorf("message = %q, want the CLI's own words", outcome.Message)
	}
}

// TestClaudeCLIDropsWhatItCannotDo covers the three capabilities this backend
// has no channel for. Each is unsupported rather than unimplemented: no amount
// of work in dragoman would add them.
func TestClaudeCLIDropsWhatItCannotDo(t *testing.T) {
	body := []byte(`{"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],` +
		`"tools":[{"type":"function","name":"get_weather"}],"tool_choice":"required",` +
		`"parallel_tool_calls":false,"max_output_tokens":512}`)

	sent, drops := translate(t, core.Options{Preset: "claude-cli"}, body)
	for _, path := range []string{"tools[0]", "tool_choice", "parallel_tool_calls", "max_output_tokens"} {
		if !hasDropPath(drops, path) {
			t.Errorf("%s was not reported: %v", path, drops)
		}
	}
	for _, drop := range drops {
		if drop.Reason != base.DropUnsupported {
			t.Errorf("%s = %q, want %q", drop.Path, drop.Reason, base.DropUnsupported)
		}
	}
	// Nothing may reach the CLI that it would silently ignore.
	for _, field := range []string{"tools", "tool_choice", "max_tokens"} {
		if lookup(sent, field) != "" {
			t.Errorf("%s was sent to a backend that cannot act on it: %v", field, sent)
		}
	}
}

// TestClaudeCLIStrictRefusesTheWeatherRequest is the parent spec's live request
// meeting the first backend that cannot serve it. Silence is not an option: a
// tool-using client gets prose with an invented answer in it otherwise.
func TestClaudeCLIStrictRefusesTheWeatherRequest(t *testing.T) {
	_, err := core.Run(context.Background(), catalog.New(),
		core.Options{Preset: "claude-cli", Strict: true, NoExecute: true}, weatherRequest(t), nil, nil)
	if err == nil {
		t.Fatal("strict accepted a request whose only purpose is a tool call")
	}
	if core.CodeOf(err) != core.ExitUsage {
		t.Errorf("exit = %d, want %d", core.CodeOf(err), core.ExitUsage)
	}
	if !strings.Contains(err.Error(), "tools[0]") {
		t.Errorf("error does not name the field with its path: %v", err)
	}
}

// TestClaudeCLIRefusesASecondUserTurn covers the one restriction that is not a
// drop. Dropping a user message would change the conversation and sending it
// would run an extra turn whose answer nobody asked for.
func TestClaudeCLIRefusesASecondUserTurn(t *testing.T) {
	const user = `{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}`
	const assistant = `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}`
	const call = `{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"}`
	const output = `{"type":"function_call_output","call_id":"c1","output":"ok"}`
	const system = `{"type":"message","role":"system","content":[{"type":"input_text","text":"be terse"}]}`

	cases := []struct {
		name    string
		items   []string
		refused bool
	}{
		{name: "one turn", items: []string{user}},
		{name: "instructions do not open a turn", items: []string{system, user}},
		{name: "history ending in one question", items: []string{assistant, user}},
		// A tool result is a user turn to this backend, so a history that also
		// carries the question that caused the call is two.
		{name: "history ending in a tool result", items: []string{assistant, call, output}},
		{name: "the question that caused the call is a second turn", items: []string{user, call, output}, refused: true},
		{name: "two questions", items: []string{user, assistant, user}, refused: true},
		{name: "a tool result then a question", items: []string{call, output, assistant, user}, refused: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			body := []byte(`{"input":[` + strings.Join(testCase.items, ",") + `]}`)
			_, err := core.Run(context.Background(), catalog.New(),
				core.Options{Preset: "claude-cli", NoExecute: true}, body, nil, nil)
			if !testCase.refused {
				if err != nil {
					t.Fatalf("a single turn was refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("a second user turn was accepted")
			}
			if core.CodeOf(err) != core.ExitUsage {
				t.Errorf("exit = %d, want %d", core.CodeOf(err), core.ExitUsage)
			}
			if !strings.Contains(err.Error(), "input[") {
				t.Errorf("error does not name the turns with their paths: %v", err)
			}
		})
	}
}

// TestClaudeCLINeedsNoCredential covers the backend that authenticates itself.
// Every other backend names an environment variable and is refused without it;
// this one was signed in once, interactively, and there is no key to find.
func TestClaudeCLINeedsNoCredential(t *testing.T) {
	backend, err := core.SelectBackend(catalog.New(), core.Options{Preset: "claude-cli"})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	if backend.KeyEnv != "" {
		t.Errorf("KeyEnv = %q, want none: the CLI holds its own credential", backend.KeyEnv)
	}
	// The dialect's own variable must not be inherited, or a developer with a
	// subscription and no API key could not use this backend at all.
	if backend.KeyEnv == "ANTHROPIC_API_KEY" {
		t.Error("the dialect's key requirement leaked onto a self-authenticating backend")
	}
}

// TestExecEndpointCarriesItsOwnLimits keeps the table a convenience. What this
// backend does differently follows from how it is reached, so naming the binary
// in flags behaves exactly as the table entry does — the alternative is a flag
// form that silently sends tools into a void.
func TestExecEndpointCarriesItsOwnLimits(t *testing.T) {
	viaFlags, err := core.SelectBackend(catalog.New(), core.Options{
		BackendProtocol: "anthropic-messages",
		Endpoint:        "exec:claude",
	})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	viaTable, err := core.SelectBackend(catalog.New(), core.Options{Preset: "claude-cli"})
	if err != nil {
		t.Fatalf("select: %v", err)
	}
	// The whole backend rather than the three limits by name, so a capability
	// added later cannot reach one form and not the other. Only what the
	// service was called differs.
	viaFlags.Name = viaTable.Name
	if viaFlags != viaTable {
		t.Errorf("flags and table disagree\nflags: %+v\ntable: %+v", viaFlags, viaTable)
	}
	if !viaFlags.NoTools || !viaFlags.NoOutputCap || !viaFlags.OneTurnPerRequest {
		t.Errorf("backend = %+v, want every limit of reaching a CLI", viaFlags)
	}
	// The CLI has no buffered form, so the executor's Do is unreachable only
	// while the protocol it speaks is streamed upstream.
	if !catalog.Dialects()[viaFlags.Dialect].StreamUpstream {
		t.Error("the protocol stopped streaming upstream; a buffered call would reach a CLI that cannot serve one")
	}
}

// TestClaudeCLIDecodesTheRecordedStream is the offline half of the headline
// criterion: a plain knowledge question, answered, in a well-formed
// openai-responses body, from frames the installed CLI actually emitted.
func TestClaudeCLIDecodesTheRecordedStream(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		mode := "buffered"
		if streamed {
			mode = "streamed"
		}
		t.Run(mode, func(t *testing.T) {
			events := []base.Event{}
			sink := func(event base.Event) error {
				events = append(events, event)
				return nil
			}
			result, err := core.Run(context.Background(), catalog.New(),
				core.Options{Preset: "claude-cli", Model: "sonnet", Stream: &streamed},
				knowledgeRequest(t), spawned{file: "stream.ndjson"}, sink)
			if err != nil {
				t.Fatalf("run: %v", err)
			}

			answer := base.Response{}
			if streamed {
				assertEventGrammar(t, events)
				folded, foldErr := base.Fold(events)
				if foldErr != nil {
					t.Fatalf("fold: %v", foldErr)
				}
				answer = folded
			} else {
				if result.Response == nil {
					t.Fatal("buffered run returned no response")
				}
				answer = *result.Response
			}

			if answer.Status != base.StatusCompleted {
				t.Errorf("status = %q, want %q", answer.Status, base.StatusCompleted)
			}
			if !strings.Contains(base.FindText(answer), "Rio de Janeiro") {
				t.Errorf("answer = %q, want the city", base.FindText(answer))
			}
			if answer.Usage == nil || answer.Usage.InputTokens == 0 {
				t.Errorf("usage = %+v, want the counts the CLI reported", answer.Usage)
			}
			if len(result.Drops) != 0 {
				t.Errorf("drops = %v, want none: the request carries nothing this backend cannot do", result.Drops)
			}
		})
	}
}

// TestClaudeCLICarriesProviderState covers the field that survives a turn
// boundary only in the client's hands. A reasoning item arrives with its
// signature in the pocket, and goes back out in position on the next request —
// the CLI rejects a modified one, so this is the check that a history built
// from this backend can be replayed to it.
func TestClaudeCLICarriesProviderState(t *testing.T) {
	answered, err := core.Run(context.Background(), catalog.New(),
		core.Options{Preset: "claude-cli", Model: "sonnet"},
		knowledgeRequest(t), spawned{file: "reasoning.ndjson"}, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if answered.Response == nil {
		t.Fatal("no response")
	}

	reasoning := base.FilterItems(answered.Response.Output, func(item base.Item) bool {
		return item.Type == base.ItemReasoning
	})
	if len(reasoning) != 1 {
		t.Fatalf("reasoning items = %d, want 1", len(reasoning))
	}
	state := reasoning[0].EncryptedContent
	if state == "" {
		t.Fatal("the pocket is empty; the next request would be rejected")
	}

	// Echoed back, the state has to reach the wire byte for byte and in
	// position — on the assistant turn it arrived on, not in a bag on the
	// document.
	echoed, err := json.Marshal(map[string]any{"input": answered.Response.Output})
	if err != nil {
		t.Fatalf("echo: %v", err)
	}
	sent, drops := translate(t, core.Options{Preset: "claude-cli", Model: "sonnet"}, echoed)
	if len(drops) != 0 {
		t.Errorf("drops = %v, want none: a history from this backend replays to it", drops)
	}
	if got := lookup(sent, "messages.0.content.0.signature"); got != state {
		t.Errorf("signature was altered\n got: %q\nwant: %q", got, state)
	}
	if got := lookup(sent, "messages.0.role"); got != "assistant" {
		t.Errorf("state rode a %q turn, want assistant", got)
	}
}

// stubCLI writes an executable standing in for the CLI. It is a real process
// reached the real way — spawned, handed stdin, read back over a pipe — so the
// executor is exercised rather than replaced by a test double.
func stubCLI(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stub is a shell script")
	}
	path := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatalf("stub: %v", err)
	}
	return path
}

// signedIn is the auth branch every stub needs, since preflight runs first.
const signedIn = `if [ "$1" = "auth" ]; then echo '{"loggedIn":true}'; exit 0; fi
`

// TestClaudeCLIExecutorSpawnsAndReports drives the whole executor: preflight,
// the spawn, the pipe, the terminal frame and the exit code. Each row is a way
// a locally installed binary can fail that no HTTP backend can.
func TestClaudeCLIExecutorSpawnsAndReports(t *testing.T) {
	answered := signedIn + "cat " + filepath.Join("..", "..", "testdata", "fixtures", "claude-cli", "stream.ndjson")

	cases := []struct {
		name    string
		script  string
		missing bool
		code    core.ExitCode
		says    string
	}{
		{name: "answers", script: answered, code: core.ExitOK},
		{
			// The binary is the one thing a user is most likely not to have.
			name: "not installed", missing: true, code: core.ExitUsage, says: "not found on PATH",
		},
		{
			name:   "signed out",
			script: `if [ "$1" = "auth" ]; then echo '{"loggedIn":false}'; exit 0; fi` + "\necho unreachable",
			code:   core.ExitUsage, says: "not authenticated",
		},
		{
			name:   "auth status is not readable",
			script: `if [ "$1" = "auth" ]; then echo 'not json'; exit 0; fi` + "\necho unreachable",
			code:   core.ExitUsage, says: "not authenticated",
		},
		{
			// The backend was reached and did not answer, which is one, not two.
			name:   "the process fails",
			script: signedIn + `echo "something went wrong" >&2` + "\nexit 3",
			code:   core.ExitBackend, says: "something went wrong",
		},
		{
			name:   "the stream ends with no result",
			script: signedIn + `echo '{"type":"system","subtype":"init"}'`,
			code:   core.ExitBackend, says: "without a result",
		},
		{
			// Failure arrives in the stream rather than in an exit status.
			name: "the CLI reports an error frame",
			script: signedIn +
				`echo '{"is_error":true,"api_error_status":404,"result":"no such model","type":"result"}'`,
			code: core.ExitBackend, says: "404",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			binary := filepath.Join(t.TempDir(), "claude")
			if !testCase.missing {
				binary = stubCLI(t, testCase.script)
			}
			_, err := core.Run(context.Background(), catalog.New(), core.Options{
				BackendProtocol: "anthropic-messages",
				Endpoint:        "exec:" + binary,
				Model:           "sonnet",
			}, knowledgeRequest(t), nil, nil)

			if core.CodeOf(err) != testCase.code {
				t.Fatalf("exit = %d, want %d (err: %v)", core.CodeOf(err), testCase.code, err)
			}
			if testCase.says != "" && !strings.Contains(err.Error(), testCase.says) {
				t.Errorf("error does not say %q: %v", testCase.says, err)
			}
		})
	}
}

// TestClaudeCLISurvivesAnUnreadStdin covers the deadlock a subprocess backend
// can have and an HTTP one cannot: a child that answers without draining its
// input, while the request is larger than a pipe buffer.
func TestClaudeCLISurvivesAnUnreadStdin(t *testing.T) {
	binary := stubCLI(t, signedIn+"cat "+
		filepath.Join("..", "..", "testdata", "fixtures", "claude-cli", "stream.ndjson"))

	prompt := strings.Repeat("lorem ipsum dolor sit amet ", 40000)
	body, err := json.Marshal(map[string]any{"input": []base.Item{
		base.TextItem(base.RoleUser, base.PartInputText, prompt),
	}})
	if err != nil {
		t.Fatalf("body: %v", err)
	}

	result, err := core.Run(context.Background(), catalog.New(), core.Options{
		BackendProtocol: "anthropic-messages",
		Endpoint:        "exec:" + binary,
		Model:           "sonnet",
	}, body, nil, nil)
	if err != nil {
		t.Fatalf("a child that ignored a %d byte stdin failed the call: %v", len(body), err)
	}
	if result.Response == nil || !strings.Contains(base.FindText(*result.Response), "Rio de Janeiro") {
		t.Errorf("answer = %+v, want the recorded one", result.Response)
	}
}

// TestClaudeCLIDoesNotHangOnAnUnreadableFrame covers the failure a subprocess
// backend has and an HTTP one cannot. When the decoder rejects a frame the
// reader stops, and a child still writing into a pipe nobody drains will block
// on a full one — taking Wait, and dragoman, down with it. The diagnostic that
// was about to be printed must arrive instead of a hang.
func TestClaudeCLIDoesNotHangOnAnUnreadableFrame(t *testing.T) {
	// A text block whose text is an array: a shape the dialect cannot read, so
	// the decoder errors on the first frame and everything after it is unread.
	binary := stubCLI(t, signedIn+
		`echo '{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":["not","a","string"]}}}'`+"\n"+
		// More than a pipe buffer holds, so the child blocks writing it.
		`head -c 1000000 /dev/zero`+"\n")

	done := make(chan error, 1)
	go func() {
		_, err := core.Run(context.Background(), catalog.New(), core.Options{
			BackendProtocol: "anthropic-messages",
			Endpoint:        "exec:" + binary,
			Model:           "sonnet",
		}, knowledgeRequest(t), nil, nil)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("an unreadable frame was accepted")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("dragoman hung on a child writing into a pipe it had stopped reading")
	}
}

// TestClaudeCLIBufferedCallIsNamed covers the branch the quirks record keeps
// unreachable. The CLI has no buffered form, so a Do against it must say so
// rather than return the empty body that would fold into an empty answer.
func TestClaudeCLIBufferedCallIsNamed(t *testing.T) {
	body, err := core.NewClaudeCLIExecutor().Do(context.Background(), core.Call{URL: "exec:claude"})
	if err == nil {
		t.Fatal("a buffered call returned no error")
	}
	if body != nil {
		t.Errorf("body = %s, want none", body)
	}
	if !strings.Contains(err.Error(), "stream") {
		t.Errorf("error does not name the reason: %v", err)
	}
}

func contains(argv []string, value string) bool { return slices.Contains(argv, value) }
