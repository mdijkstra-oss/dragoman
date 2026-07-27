package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/matthijn/dragoman/internal/base"
)

// waitDelay bounds how long Wait may spend on a child that was killed but left
// a pipe open. It is a backstop against hanging, not a timeout on the answer:
// the request itself may legitimately run for as long as the model takes.
const waitDelay = 5 * time.Second

// ClaudeCLIExecutor reaches a locally installed Claude Code CLI. It speaks the
// same dialect as the HTTP Anthropic endpoint — the CLI's stream carries
// anthropic-messages events verbatim inside an envelope — so only the reaching
// differs, never the translation.
//
// The CLI is an agent by default: it reads CLAUDE.md, runs hooks, resolves
// skills and plugins, persists a session and executes tools. A caller who sent
// a Responses body asked for none of that, so the flags that turn it back into
// a model endpoint are fixed here and are not configurable.
type ClaudeCLIExecutor struct{}

func NewClaudeCLIExecutor() ClaudeCLIExecutor { return ClaudeCLIExecutor{} }

// Do is unreachable while the dialect streams upstream: the CLI has no buffered
// form, so there is no body to return. It exists so that losing that property is
// a named failure rather than a silent empty answer.
func (e ClaudeCLIExecutor) Do(_ context.Context, _ Call) (json.RawMessage, error) {
	return nil, Fail(ExitUsage,
		"the claude CLI answers only as a stream; this backend must be streamed upstream")
}

func (e ClaudeCLIExecutor) Stream(ctx context.Context, call Call, onFrame func(base.Frame) error) error {
	binary, err := BinaryOf(call.URL)
	if err != nil {
		return err
	}
	if err := Preflight(ctx, binary); err != nil {
		return err
	}
	argv, stdin, err := Command(call.Body)
	if err != nil {
		return err
	}

	// A reader that stops early — a frame the decoder cannot read, a stdout
	// that will not take an event — leaves the child writing into a pipe
	// nobody drains. It blocks on a full pipe, Wait blocks on it, and dragoman
	// hangs instead of printing the diagnostic it was about to print. So the
	// context has to be one this call can cancel.
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	command := exec.CommandContext(ctx, binary, argv...)
	command.Stdin = bytes.NewReader(stdin)
	// A killed child can still leave a pipe open behind it, and Wait would
	// then wait on the pipe rather than on the process.
	command.WaitDelay = waitDelay
	diagnostics := &bytes.Buffer{}
	command.Stderr = diagnostics
	stdout, err := command.StdoutPipe()
	if err != nil {
		return Fail(ExitBackend, "reading from %s: %s", binary, err)
	}
	if err := command.Start(); err != nil {
		return Fail(ExitBackend, "starting %s: %s", binary, err)
	}

	outcome, readErr := ReadCLIFrames(stdout, onFrame)
	if readErr != nil {
		stop()
	}
	// Wait even when the read failed: the child still has to be reaped, and
	// the error the reader produced is the one worth reporting.
	waitErr := command.Wait()

	if readErr != nil {
		return readErr
	}
	if outcome.Failed {
		return Fail(ExitBackend, "the claude CLI reported %s: %s", outcome.Status, outcome.Message)
	}
	if waitErr != nil {
		return Fail(ExitBackend, "%s: %s%s", binary, waitErr, detail(diagnostics))
	}
	if !outcome.Seen {
		return Fail(ExitBackend, "%s ended without a result%s", binary, detail(diagnostics))
	}
	return nil
}

// Preflight checks the two things that must not be discovered mid-request: that
// the CLI is installed, and that it is signed in. Both are usage failures
// because a binary that is absent or logged out never got asked, and exit 1 is
// reserved for a backend that said no. Reading the status costs no tokens and
// makes no model call.
func Preflight(ctx context.Context, binary string) error {
	path, err := exec.LookPath(binary)
	if err != nil {
		return Fail(ExitUsage,
			"claude-cli backend: %q not found on PATH; install Claude Code or pass --endpoint exec:/path/to/claude", binary)
	}
	reported, err := exec.CommandContext(ctx, path, "auth", "status", "--json").Output()
	if err != nil {
		return Fail(ExitUsage,
			`claude-cli backend: could not read authentication status from %s; run "claude auth login"`, binary)
	}
	var status struct {
		LoggedIn bool `json:"loggedIn"`
	}
	if err := json.Unmarshal(reported, &status); err != nil || !status.LoggedIn {
		return Fail(ExitUsage, `claude-cli backend: not authenticated; run "claude auth login"`)
	}
	return nil
}

// Command builds the argv and the stdin bytes for one call out of the
// anthropic-messages body encoded for it. It performs no IO, so the whole
// mapping from a request to an invocation is table-testable without spawning
// anything.
func Command(body []byte) ([]string, []byte, error) {
	var request cliRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, nil, Fail(ExitUsage, "claude-cli: reading the encoded request: %s", err)
	}

	argv := append([]string{}, opaqueArgs...)
	// The system prompt is always passed, empty if the caller sent no
	// instructions. Omitting it leaves Claude Code's own agent prompt in place,
	// which is a body of meaning the caller never asked for; passing empty is
	// not writing a prompt but declining to add one.
	argv = append(argv, "--system-prompt", request.System)
	if request.Model != "" {
		argv = append(argv, "--model", request.Model)
	}
	if config := request.OutputConfig; config != nil {
		if config.Effort != "" {
			argv = append(argv, "--effort", config.Effort)
		}
		if config.Format != nil && len(config.Format.Schema) > 0 {
			argv = append(argv, "--json-schema", string(config.Format.Schema))
		}
	}

	stdin := []byte(nil)
	for _, message := range request.Messages {
		line, err := json.Marshal(cliLine{Type: message.Role, Message: message})
		if err != nil {
			return nil, nil, Fail(ExitUsage, "claude-cli: encoding a message: %s", err)
		}
		stdin = append(append(stdin, line...), '\n')
	}
	return argv, stdin, nil
}

// BinaryOf reads the binary out of an exec endpoint. It is a name resolved on
// PATH or an absolute path, and it is never a secret, so unlike a credential it
// may legitimately arrive from a flag.
func BinaryOf(endpoint string) (string, error) {
	binary := strings.TrimPrefix(endpoint, base.SchemeExec+":")
	if binary == endpoint || binary == "" {
		return "", Fail(ExitUsage,
			"claude-cli backend: endpoint %q does not name a binary; use exec:claude", endpoint)
	}
	return binary, nil
}

// CLIOutcome is what the CLI's terminal frame reported. Failure arrives in the
// stream rather than in an exit status, so it has to be read out of the last
// frame rather than waited for.
type CLIOutcome struct {
	Seen    bool
	Failed  bool
	Status  string
	Message string
}

// ReadCLIFrames unwraps the CLI's own framing and hands the dialect events on.
// Everything else the CLI emits — session bookkeeping, hook lifecycle, rate
// limits — is not model output and is discarded rather than decoded. An
// unrecognised frame is discarded too: the CLI adds frame types between
// releases and a new one must not break a translation.
func ReadCLIFrames(reader io.Reader, onFrame func(base.Frame) error) (CLIOutcome, error) {
	outcome := CLIOutcome{}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var frame cliFrame
		if err := json.Unmarshal(line, &frame); err != nil {
			continue
		}
		switch frame.Type {
		case "stream_event":
			if len(frame.Event) == 0 {
				continue
			}
			if err := onFrame(base.Frame{Data: frame.Event}); err != nil {
				return outcome, err
			}
		case "result":
			outcome = resultOutcome(frame)
		}
	}
	if err := scanner.Err(); err != nil {
		return outcome, Fail(ExitBackend, "reading the claude CLI stream: %s", err)
	}
	return outcome, nil
}

// opaqueArgs is what makes the CLI a model endpoint rather than an agent in a
// working directory. --safe-mode is the load-bearing one: without it the
// caller's request is answered with the local CLAUDE.md prepended to it. It is
// preferred to --bare, which suppresses more but forces an API key and so
// removes the only reason to reach the CLI at all.
var opaqueArgs = []string{
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
}

func resultOutcome(frame cliFrame) CLIOutcome {
	outcome := CLIOutcome{Seen: true, Failed: frame.IsError, Message: frame.Result}
	outcome.Status = "an error"
	if frame.APIErrorStatus != nil {
		outcome.Status = fmt.Sprintf("HTTP %d", *frame.APIErrorStatus)
	}
	return outcome
}

func detail(diagnostics *bytes.Buffer) string {
	text := strings.TrimSpace(diagnostics.String())
	if text == "" {
		return ""
	}
	return ": " + text
}

// cliRequest reads back the fields of the encoded anthropic-messages body that
// the CLI takes as flags or as stdin. Message content is kept as raw JSON so
// that every block the dialect built — thinking with its signature, a tool call,
// a tool result — reaches the CLI byte for byte.
type cliRequest struct {
	Model        string           `json:"model"`
	System       string           `json:"system"`
	Messages     []cliMessage     `json:"messages"`
	OutputConfig *cliOutputConfig `json:"output_config"`
}

type cliMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type cliOutputConfig struct {
	Effort string     `json:"effort"`
	Format *cliFormat `json:"format"`
}

type cliFormat struct {
	Schema json.RawMessage `json:"schema"`
}

// cliLine is one line of the CLI's stdin. Its type repeats the message's role,
// which is how the CLI distinguishes a turn to run from history to absorb.
type cliLine struct {
	Type    string     `json:"type"`
	Message cliMessage `json:"message"`
}

type cliFrame struct {
	Type           string          `json:"type"`
	Event          json.RawMessage `json:"event"`
	IsError        bool            `json:"is_error"`
	APIErrorStatus *int            `json:"api_error_status"`
	Result         string          `json:"result"`
}
