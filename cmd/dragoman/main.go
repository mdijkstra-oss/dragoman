// Command dragoman translates an openai-responses request to a backend
// speaking another dialect and translates the answer back, so the client never
// learns which provider answered.
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/alecthomas/kong"

	"github.com/matthijn/dragoman/internal/catalog"
	"github.com/matthijn/dragoman/internal/core"
	"github.com/matthijn/dragoman/internal/dialect/openairesponses"
	"github.com/matthijn/dragoman/internal/transport/pipe"
)

// CLI is the command line. It owns argv parsing, reading and writing the
// streams, event framing, and the exit code — and owns nothing else.
type CLI struct {
	Protocol        string `help:"Dialect the caller speaks." default:"openai-responses" enum:"openai-responses"`
	BackendProtocol string `help:"Dialect the backend speaks." name:"backend-protocol"`
	Endpoint        string `help:"Backend base URL."`
	Backend         string `help:"Named backend preset."`

	Model     string `help:"Model to request. Passed through untouched."`
	Effort    string `help:"Reasoning level. Passed through untouched."`
	MaxTokens int    `help:"Output cap." name:"max-tokens" default:"0"`

	Stream    *bool `help:"Deliver the event sequence." negatable:""`
	Strict    bool  `help:"Treat any dropped field as an error."`
	NoExecute bool  `help:"Translate and print the request; make no call." name:"no-execute"`
	Raw       bool  `help:"Return the backend's untranslated response."`
	Verbose   bool  `help:"Write the translation and its dropped fields to stderr." short:"v"`

	File   string `help:"Read the request from a file instead of stdin." short:"f" type:"path"`
	Output string `help:"Write the result to a file instead of stdout." short:"o" type:"path"`
}

func main() {
	os.Exit(run())
}

func run() int {
	cli := CLI{}
	parser, err := kong.New(&cli,
		kong.Name("dragoman"),
		kong.Description("A stateless protocol-translating LLM proxy.\n\n"+
			"Reads an openai-responses request and returns an openai-responses response,\n"+
			"whichever dialect the backend speaks. Credentials come from the environment."),
		kong.UsageOnError(),
		// kong's own failure path exits 1, which is reserved here for "the
		// backend said no". Bad flags are a dragoman failure and must be 2.
		kong.Exit(func(code int) { panic(usageExit{code: code}) }),
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dragoman: %s\n", err)
		return int(core.ExitUsage)
	}

	code, parsed := parse(parser, os.Args[1:])
	if !parsed {
		return code
	}

	options, err := options(cli)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dragoman: %s\n", err)
		return int(core.ExitUsage)
	}

	input, err := pipe.OpenInput(cli.File)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dragoman: %s\n", err)
		return int(core.ExitUsage)
	}
	defer input.Close()

	output, err := pipe.OpenOutput(cli.Output)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dragoman: %s\n", err)
		return int(core.ExitUsage)
	}
	defer output.Close()

	buffered := bufio.NewWriter(output)
	defer buffered.Flush()

	streams := pipe.Streams{In: input, Out: buffered, Err: os.Stderr}
	executor := core.NewHTTPExecutor()
	return pipe.Run(context.Background(), catalog.New(), options, streams, executor, cli.Verbose)
}

// parse runs kong and converts its exit into dragoman's own, so that a bad
// flag never surfaces as the code reserved for a backend refusal.
func parse(parser *kong.Kong, args []string) (code int, parsed bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			exit, isUsage := recovered.(usageExit)
			if !isUsage {
				panic(recovered)
			}
			// Help and version leave through kong's zero; anything else is a
			// dragoman failure and must not borrow the backend's code.
			if exit.code == 0 {
				code, parsed = int(core.ExitOK), false
				return
			}
			code, parsed = int(core.ExitUsage), false
		}
	}()
	if _, err := parser.Parse(args); err != nil {
		parser.Errorf("%s", err)
		return int(core.ExitUsage), false
	}
	return int(core.ExitOK), true
}

func options(cli CLI) (core.Options, error) {
	if cli.Protocol != openairesponses.Name {
		return core.Options{}, fmt.Errorf("unknown protocol %q", cli.Protocol)
	}
	if cli.Backend == "" && cli.BackendProtocol == "" {
		return core.Options{}, fmt.Errorf(
			"no backend: pass --backend (%s) or --backend-protocol (%s) with --endpoint",
			strings.Join(catalog.PresetNames(), ", "),
			strings.Join(catalog.DialectNames(), ", "))
	}
	opts := core.Options{
		Protocol:        cli.Protocol,
		BackendProtocol: cli.BackendProtocol,
		Endpoint:        cli.Endpoint,
		Preset:          cli.Backend,
		Model:           cli.Model,
		Effort:          cli.Effort,
		Strict:          cli.Strict,
		NoExecute:       cli.NoExecute,
		Raw:             cli.Raw,
	}
	if cli.MaxTokens > 0 {
		cap := cli.MaxTokens
		opts.MaxTokens = &cap
	}
	opts.Stream = cli.Stream
	return opts, nil
}

// usageExit is how kong's exit is caught so dragoman owns the code.
type usageExit struct{ code int }
