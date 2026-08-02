// Command dragoman translates an openai-responses request to a backend
// speaking another protocol and translates the answer back, so the client never
// learns which provider answered. It does that once over a pipe, or for as long
// as it runs over HTTP.
package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/alecthomas/kong"

	"github.com/mdijkstra-oss/dragoman/internal/catalog"
	"github.com/mdijkstra-oss/dragoman/internal/config"
	"github.com/mdijkstra-oss/dragoman/internal/core"
	"github.com/mdijkstra-oss/dragoman/internal/dialect/openairesponses"
	"github.com/mdijkstra-oss/dragoman/internal/transport/pipe"
	"github.com/mdijkstra-oss/dragoman/internal/transport/serve"
)

// CLI is the command line. It owns argv parsing, reading and writing the
// streams, and the exit code — and owns nothing else.
type CLI struct {
	Translate TranslateCmd `cmd:"" default:"withargs" help:"Translate one request from stdin to stdout."`
	Serve     ServeCmd     `cmd:"" help:"Serve the service table over HTTP."`
	Config    ConfigCmd    `cmd:"" help:"Print the service table this binary would serve."`
	Health    HealthCmd    `cmd:"" name:"healthcheck" help:"Report whether a listener is serving."`
}

// TranslateCmd is the original command line, unchanged: it stays the default so
// that a pipe invocation never has to name it.
type TranslateCmd struct {
	Protocol        string `help:"Protocol the caller speaks." default:"openai-responses" enum:"openai-responses"`
	BackendProtocol string `help:"Protocol the backend speaks." name:"backend-protocol"`
	Endpoint        string `help:"Backend base URL."`
	Backend         string `help:"Named service from the table."`

	Model     string `help:"Model to request. Passed through untouched."`
	Effort    string `help:"Reasoning level. Passed through untouched."`
	MaxTokens int    `help:"Output cap." name:"max-tokens" default:"0"`

	Stream    *bool `help:"Deliver the event sequence." negatable:""`
	Strict    bool  `help:"Treat any dropped field as an error."`
	NoExecute bool  `help:"Translate and print the request; make no call." name:"no-execute"`
	Raw       bool  `help:"Return the backend's untranslated response."`
	Verbose   bool  `help:"Write the translation and every dropped field to stderr." short:"v"`
	Timing    bool  `help:"Write the elapsed time to stderr."`

	File   string `help:"Read the request from a file instead of stdin." short:"f" type:"path"`
	Output string `help:"Write the result to a file instead of stdout." short:"o" type:"path"`

	ConfigFile string `help:"Service table to use instead of the built-in one." name:"config" type:"path"`
}

// ServeCmd is the HTTP surface over the same core. It binds loopback by
// default: an instance holding provider keys that needs to be reachable belongs
// behind something that does authentication as its actual job.
type ServeCmd struct {
	Addr       string `help:"Address to listen on." default:"127.0.0.1:8080"`
	ConfigFile string `help:"Service table to serve instead of the built-in one." name:"config" type:"path"`
	Strict     bool   `help:"Treat any dropped field as an error, for every request served."`
}

// HealthCmd lets the binary check itself. A container built on scratch holds one
// file and no shell, so a runtime healthcheck naming any other command names one
// the image cannot execute — and a deployment that cannot gate on readiness sends
// its first requests to a process that has not finished binding.
//
// It asks only whether this process is serving. Reaching a provider is deliberately
// not part of the answer: an instance that reports itself unhealthy when a backend
// is having a bad afternoon gets restarted for someone else's outage.
type HealthCmd struct {
	Addr string `help:"Address to check." default:"127.0.0.1:8080"`
}

type ConfigCmd struct {
	ConfigFile string `help:"Print this table instead of the built-in one." name:"config" type:"path"`
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
			"whichever protocol the backend speaks. Credentials come from the environment."),
		kong.UsageOnError(),
		// kong's own failure path exits 1, which is reserved here for "the
		// backend said no". Bad flags are a dragoman failure and must be 2.
		kong.Exit(func(code int) { panic(usageExit{code: code}) }),
	)
	if err != nil {
		return fail(err)
	}

	command, code, parsed := parse(parser, os.Args[1:])
	if !parsed {
		return code
	}

	switch command {
	case "serve":
		return serveCommand(cli.Serve)
	case "config":
		return configCommand(cli.Config)
	case "healthcheck":
		return healthCommand(cli.Health)
	default:
		return translateCommand(cli.Translate)
	}
}

func translateCommand(cli TranslateCmd) int {
	options, err := options(cli)
	if err != nil {
		return fail(err)
	}
	known, err := table(cli.ConfigFile)
	if err != nil {
		return fail(err)
	}

	input, err := pipe.OpenInput(cli.File)
	if err != nil {
		return fail(err)
	}
	defer input.Close()

	output, err := pipe.OpenOutput(cli.Output)
	if err != nil {
		return fail(err)
	}
	defer output.Close()

	buffered := bufio.NewWriter(output)
	defer buffered.Flush()

	streams := pipe.Streams{In: input, Out: buffered, Err: os.Stderr}
	// How a backend is reached follows from its endpoint, which the core
	// resolves against the table; the CLI owns argv, the streams and the exit
	// code, and nothing else.
	return pipe.Run(context.Background(), known, options, streams, nil,
		pipe.Report{Verbose: cli.Verbose, Timing: cli.Timing})
}

func serveCommand(cli ServeCmd) int {
	known, err := table(cli.ConfigFile)
	if err != nil {
		return fail(err)
	}
	logged, err := serve.LoggedHeaders()
	if err != nil {
		return fail(err)
	}
	// JSON, because the accounting line is the thing a deployment aggregates,
	// and a format only a human reads is a format nothing counts.
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	handler := serve.New(serve.Config{
		Catalog:    known,
		Strict:     cli.Strict,
		Log:        log,
		LogHeaders: logged,
	})
	log.Info("serving", "services", names(known), "addr", cli.Addr)

	// No write timeout: an answer may legitimately run long, and a deadline
	// here would truncate a stream the caller is still reading.
	server := &http.Server{Addr: cli.Addr, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	if err := server.ListenAndServe(); err != nil {
		return fail(err)
	}
	return int(core.ExitOK)
}

func names(known core.Catalog) []string {
	served := make([]string, 0, len(known.Presets))
	for name := range known.Presets {
		served = append(served, name)
	}
	sort.Strings(served)
	return served
}

// healthCommand asks the liveness route the question a runtime asks, and answers in
// the only vocabulary a runtime reads: the exit code. The timeout is short because
// the question is whether a listener answers, not whether it answers quickly.
func healthCommand(cli HealthCmd) int {
	client := &http.Client{Timeout: healthTimeout}
	response, err := client.Get("http://" + cli.Addr + "/health")
	if err != nil {
		return unhealthy(fmt.Errorf("reach %s: %w", cli.Addr, err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return unhealthy(fmt.Errorf("%s answered %s", cli.Addr, response.Status))
	}
	return int(core.ExitOK)
}

// healthTimeout is short because the question is whether a listener answers, not
// whether it answers quickly. A check that outlasts the interval it runs on stops
// reporting the state it was asked about.
const healthTimeout = 2 * time.Second

// unhealthy answers in the only vocabulary a container runtime reads. The code is
// neither of dragoman's own: a listener that is not up is not a usage error, and
// nothing here asked a backend anything. One is what every runtime reads as
// unhealthy, and the only value that has ever meant it.
func unhealthy(err error) int {
	fmt.Fprintf(os.Stderr, "dragoman: %s\n", err)
	return 1
}

func configCommand(cli ConfigCmd) int {
	document := config.Default
	if cli.ConfigFile != "" {
		read, err := os.ReadFile(cli.ConfigFile)
		if err != nil {
			return fail(err)
		}
		document = read
	}
	// Parsed before it is printed, so that printing a table is also a way to
	// find out whether dragoman can serve it.
	if _, err := catalog.Load(document); err != nil {
		return fail(err)
	}
	os.Stdout.Write(document)
	return int(core.ExitOK)
}

// table resolves the service table an invocation runs against. A file replaces
// the built-in one wholesale: the file in front of you is the whole answer to
// what this instance serves.
func table(path string) (core.Catalog, error) {
	if path == "" {
		return catalog.New(), nil
	}
	document, err := os.ReadFile(path)
	if err != nil {
		return core.Catalog{}, err
	}
	services, err := catalog.Load(document)
	if err != nil {
		return core.Catalog{}, err
	}
	return core.Catalog{Dialects: catalog.Dialects(), Presets: services, Executors: catalog.Executors()}, nil
}

// parse runs kong and converts its exit into dragoman's own, so that a bad flag
// never surfaces as the code reserved for a backend refusal.
func parse(parser *kong.Kong, args []string) (command string, code int, parsed bool) {
	defer func() {
		if recovered := recover(); recovered != nil {
			exit, isUsage := recovered.(usageExit)
			if !isUsage {
				panic(recovered)
			}
			// Help and version leave through kong's zero; anything else is a
			// dragoman failure and must not borrow the backend's code.
			if exit.code == 0 {
				command, code, parsed = "", int(core.ExitOK), false
				return
			}
			command, code, parsed = "", int(core.ExitUsage), false
		}
	}()
	context, err := parser.Parse(args)
	if err != nil {
		parser.Errorf("%s", err)
		return "", int(core.ExitUsage), false
	}
	return strings.Fields(context.Command())[0], int(core.ExitOK), true
}

func options(cli TranslateCmd) (core.Options, error) {
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
		Stream:          cli.Stream,
	}
	if cli.MaxTokens > 0 {
		cap := cli.MaxTokens
		opts.MaxTokens = &cap
	}
	return opts, nil
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "dragoman: %s\n", err)
	return int(core.CodeOf(err))
}

// usageExit is how kong's exit is caught so dragoman owns the code.
type usageExit struct{ code int }
