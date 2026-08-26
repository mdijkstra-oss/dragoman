package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// A backstop against a killed child holding a pipe open, not a timeout on the
// answer — a request may run as long as the model takes.
const killedChildPipeGrace = 5 * time.Second

// Without --safe-mode the caller's request is answered with the local CLAUDE.md
// prepended to it. --bare suppresses more but forces an API key, which removes
// the only reason to reach the CLI.
var fixedArgs = []string{
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
	"--exclude-dynamic-system-prompt-sections",
}

type Runner struct {
	Path    string
	Account string
	// The CLI reads CLAUDE.md, settings and skills from the directory it runs
	// in, so every call runs from a directory holding nothing.
	workDir string
	env     []string
}

func NewRunner(ctx context.Context, binary string) (*Runner, error) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return nil, fmt.Errorf("%q not found on PATH; install Claude Code or pass --binary", binary)
	}
	env := childEnv(os.Environ())
	account, err := signedIn(ctx, path, env)
	if err != nil {
		return nil, err
	}
	workDir, err := os.MkdirTemp("", "claude-bridge-")
	if err != nil {
		return nil, err
	}
	return &Runner{Path: path, Account: account, workDir: workDir, env: env}, nil
}

func (r *Runner) Close() { _ = os.RemoveAll(r.workDir) }

// Reading the status costs no tokens and makes no model call. Run with the
// same environment the calls run in, or this describes another account.
func signedIn(ctx context.Context, path string, env []string) (string, error) {
	check := exec.CommandContext(ctx, path, "auth", "status", "--json")
	check.Env = env
	reported, err := check.Output()
	if err != nil {
		return "", fmt.Errorf(`could not read authentication status from %s; run "claude auth login"`, path)
	}
	var status struct {
		LoggedIn     bool   `json:"loggedIn"`
		Email        string `json:"email"`
		Subscription string `json:"subscriptionType"`
	}
	if err := json.Unmarshal(reported, &status); err != nil || !status.LoggedIn {
		return "", errors.New(`not authenticated; run "claude auth login"`)
	}
	return status.Email + " (" + status.Subscription + ")", nil
}

// The CLI prefers an API key in its environment over the sign-in it already
// performed, so a key inherited from a shell profile, a CI secret or a container
// flag would silently move the call onto another account and bill it there.
// Matched by prefix because a variable added in a later release would reopen
// the hole. CLAUDE_CONFIG_DIR stays: it says where the sign-in lives.
func childEnv(environ []string) []string {
	kept := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "ANTHROPIC_") || name == "CLAUDE_CODE_USE_BEDROCK" || name == "CLAUDE_CODE_USE_VERTEX" {
			continue
		}
		kept = append(kept, entry)
	}
	return kept
}

type Invocation struct {
	Model  string
	System string
	Effort string
	Schema json.RawMessage
	// One stream-json line per message, already encoded.
	Stdin []byte
}

// Failure arrives in the CLI's stream rather than in an exit status.
type Outcome struct {
	Seen    bool
	Failed  bool
	Status  string
	Message string
}

type frame struct {
	Type           string          `json:"type"`
	Subtype        string          `json:"subtype"`
	Event          json.RawMessage `json:"event"`
	IsError        bool            `json:"is_error"`
	APIErrorStatus *int            `json:"api_error_status"`
	Result         string          `json:"result"`
}

// Run spawns one CLI call and hands every anthropic-messages event it emits to
// onEvent, in order. The returned error describes a failure of the bridge or of
// the CLI; a model-side refusal is reported through the outcome instead.
func (r *Runner) Run(ctx context.Context, call Invocation, onEvent func(json.RawMessage) error) (Outcome, error) {
	argv := append([]string{}, fixedArgs...)
	// Omitting this flag leaves Claude Code's own agent prompt in place, so it
	// is always passed, empty when the caller sent no instructions.
	argv = append(argv, "--system-prompt", call.System)
	if call.Model != "" {
		argv = append(argv, "--model", call.Model)
	}
	if call.Effort != "" {
		argv = append(argv, "--effort", call.Effort)
	}
	if len(call.Schema) > 0 {
		argv = append(argv, "--json-schema", string(call.Schema))
	}

	// A reader that stops early leaves the child writing into a pipe nobody
	// drains: it blocks on a full pipe and Wait blocks on it.
	ctx, stop := context.WithCancel(ctx)
	defer stop()

	command := exec.CommandContext(ctx, r.Path, argv...)
	command.Dir = r.workDir
	command.Env = r.env
	command.Stdin = bytes.NewReader(call.Stdin)
	command.WaitDelay = killedChildPipeGrace
	diagnostics := &bytes.Buffer{}
	command.Stderr = diagnostics
	stdout, err := command.StdoutPipe()
	if err != nil {
		return Outcome{}, fmt.Errorf("reading from %s: %w", r.Path, err)
	}
	if err := command.Start(); err != nil {
		return Outcome{}, fmt.Errorf("starting %s: %w", r.Path, err)
	}

	outcome, readErr := readFrames(stdout, onEvent)
	if readErr != nil {
		stop()
	}
	waitErr := command.Wait()

	if readErr != nil {
		return outcome, readErr
	}
	if outcome.Failed {
		return outcome, nil
	}
	if waitErr != nil {
		return outcome, fmt.Errorf("%s: %w%s", r.Path, waitErr, detail(diagnostics))
	}
	if !outcome.Seen {
		return outcome, fmt.Errorf("%s ended without a result%s", r.Path, detail(diagnostics))
	}
	return outcome, nil
}

// Session bookkeeping, hook lifecycle and rate limits are not model output. An
// unrecognised frame is discarded too, because the CLI adds frame types between
// releases.
func readFrames(reader io.Reader, onEvent func(json.RawMessage) error) (Outcome, error) {
	outcome := Outcome{}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var f frame
		if err := json.Unmarshal(line, &f); err != nil {
			continue
		}
		switch f.Type {
		case "stream_event":
			if len(f.Event) == 0 {
				continue
			}
			if err := onEvent(append(json.RawMessage(nil), f.Event...)); err != nil {
				return outcome, err
			}
		case "result":
			outcome = resultOutcome(f)
		}
	}
	if err := scanner.Err(); err != nil {
		return outcome, fmt.Errorf("reading the claude CLI stream: %w", err)
	}
	return outcome, nil
}

// A run that failed before the model answered fills in neither the status nor
// the result, and names the kind of failure in the subtype alone.
func resultOutcome(f frame) Outcome {
	outcome := Outcome{Seen: true, Failed: f.IsError, Message: strings.TrimSpace(f.Result)}
	switch {
	case f.APIErrorStatus != nil:
		outcome.Status = fmt.Sprintf("HTTP %d", *f.APIErrorStatus)
	case f.Subtype != "" && f.Subtype != "success":
		outcome.Status = f.Subtype
	default:
		outcome.Status = "an error"
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
