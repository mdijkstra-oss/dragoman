package core

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/mdijkstra-oss/dragoman/internal/base"
)

// A backstop against a killed child holding a pipe open, not a timeout on the
// answer — the request may legitimately run as long as the model takes.
const killedChildPipeGrace = 5 * time.Second

// The CLI's stream carries anthropic-messages events verbatim inside an
// envelope.
//
// It is an agent by default: it reads CLAUDE.md, runs hooks, resolves skills and
// plugins, persists a session and executes tools. The flags that turn it back
// into a model endpoint are fixed here.
type ClaudeCLIExecutor struct{}

func NewClaudeCLIExecutor() ClaudeCLIExecutor { return ClaudeCLIExecutor{} }

// The CLI has no buffered form, so there is no body to return.
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

	// A reader that stops early leaves the child writing into a pipe nobody
	// drains: it blocks on a full pipe and Wait blocks on it.
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	command := exec.CommandContext(ctx, binary, argv...)
	command.Env = ChildEnv(os.Environ())
	command.Stdin = bytes.NewReader(stdin)
	// A killed child can still leave a pipe open behind it, and Wait would
	// then wait on the pipe rather than on the process.
	command.WaitDelay = killedChildPipeGrace
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
	// The child still has to be reaped, but the reader's error is the one worth
	// reporting.
	waitErr := command.Wait()

	if readErr != nil {
		return readErr
	}
	if outcome.Failed {
		return Fail(ExitBackend, "the claude CLI reported %s%s", outcome.Status, said(outcome.Message, diagnostics))
	}
	if waitErr != nil {
		return Fail(ExitBackend, "%s: %s%s", binary, waitErr, detail(diagnostics))
	}
	if !outcome.Seen {
		return Fail(ExitBackend, "%s ended without a result%s", binary, detail(diagnostics))
	}
	return nil
}

// A binary that is absent or logged out never got asked, so neither is a backend
// refusal. Reading the status costs no tokens and makes no model call.
func Preflight(ctx context.Context, binary string) error {
	path, err := findInstalled(binary)
	if err != nil {
		return err
	}
	return checkSignedIn(ctx, path, binary)
}

func findInstalled(binary string) (string, error) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return "", Fail(ExitUsage,
			"claude-cli backend: %q not found on PATH; install Claude Code or pass --endpoint exec:/path/to/claude", binary)
	}
	return path, nil
}

func checkSignedIn(ctx context.Context, path, binary string) error {
	check := exec.CommandContext(ctx, path, "auth", "status", "--json")
	// The same environment the call will run in, or this describes an account
	// other than the one that answers.
	check.Env = ChildEnv(os.Environ())
	reported, err := check.Output()
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

func Command(body []byte) ([]string, []byte, error) {
	var request cliRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, nil, Fail(ExitUsage, "claude-cli: reading the encoded request: %s", err)
	}

	argv := append([]string{}, opaqueArgs...)
	// Omitting this flag leaves Claude Code's own agent prompt in place, so it
	// is always passed, empty when the caller sent no instructions.
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

// The CLI prefers an API key in its environment over the sign-in it already
// performed, so a key inherited from a shell profile, a CI secret or a container
// flag would silently move the call onto another account and bill it there.
//
// Matched by prefix rather than by name because a variable added in a later
// release would reopen the hole.
func ChildEnv(environ []string) []string {
	kept := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if reroutes(name) {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

// The ANTHROPIC_ namespace goes wholesale; the model, effort and schema travel
// as flags and the messages on stdin. CLAUDE_CONFIG_DIR is excluded because it
// says where the interactive credential lives, which is what is being preserved.
func reroutes(name string) bool {
	if strings.HasPrefix(name, "ANTHROPIC_") {
		return true
	}
	return name == "CLAUDE_CODE_USE_BEDROCK" || name == "CLAUDE_CODE_USE_VERTEX"
}

// A name resolved on PATH or an absolute path, never a secret.
func BinaryOf(endpoint string) (string, error) {
	binary := strings.TrimPrefix(endpoint, base.SchemeExec+":")
	if binary == endpoint || binary == "" {
		return "", Fail(ExitUsage,
			"claude-cli backend: endpoint %q does not name a binary; use exec:claude", endpoint)
	}
	return binary, nil
}

// Failure arrives in the CLI's stream rather than in an exit status, so it has
// to be read out of the last frame rather than waited for.
type CLIOutcome struct {
	Seen    bool
	Failed  bool
	Status  string
	Message string
}

// Session bookkeeping, hook lifecycle and rate limits are not model output. An
// unrecognised frame is discarded too, because the CLI adds frame types between
// releases.
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

// Without --safe-mode the caller's request is answered with the local CLAUDE.md
// prepended to it. --bare suppresses more but forces an API key, which removes
// the only reason to reach the CLI.
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

// A run that failed before the model answered fills in neither the status nor
// the result, and names the kind of failure in the subtype alone.
func resultOutcome(frame cliFrame) CLIOutcome {
	outcome := CLIOutcome{Seen: true, Failed: frame.IsError, Message: frame.Result}
	switch {
	case frame.APIErrorStatus != nil:
		outcome.Status = fmt.Sprintf("HTTP %d", *frame.APIErrorStatus)
	case frame.Subtype != "" && frame.Subtype != "success":
		outcome.Status = frame.Subtype
	default:
		outcome.Status = "an error"
	}
	return outcome
}

// A failure with an empty result tends to have said what happened on the child's
// stderr.
func said(message string, diagnostics *bytes.Buffer) string {
	spoken := []string{}
	if message = strings.TrimSpace(message); message != "" {
		spoken = append(spoken, message)
	}
	if text := strings.TrimSpace(diagnostics.String()); text != "" {
		spoken = append(spoken, text)
	}
	if len(spoken) == 0 {
		return " and gave no message"
	}
	return ": " + strings.Join(spoken, "; ")
}

func detail(diagnostics *bytes.Buffer) string {
	text := strings.TrimSpace(diagnostics.String())
	if text == "" {
		return ""
	}
	return ": " + text
}

// Message content is kept as raw JSON so that every block the dialect built —
// thinking with its signature, a tool call, a tool result — reaches the CLI byte
// for byte.
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

// The type repeats the message's role, which is how the CLI distinguishes a turn
// to run from history to absorb.
type cliLine struct {
	Type    string     `json:"type"`
	Message cliMessage `json:"message"`
}

type cliFrame struct {
	Type           string          `json:"type"`
	Subtype        string          `json:"subtype"`
	Event          json.RawMessage `json:"event"`
	IsError        bool            `json:"is_error"`
	APIErrorStatus *int            `json:"api_error_status"`
	Result         string          `json:"result"`
}
