// Command codex-bridge serves the OpenAI Responses wire format on localhost
// and answers each request with one isolated invocation of the Codex CLI.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/alecthomas/kong"
)

type CLI struct {
	Addr    string `help:"Address to listen on." default:"127.0.0.1:8083"`
	Binary  string `help:"The codex executable, a name on PATH or a path." default:"codex"`
	Verbose bool   `help:"Log every frame the CLI emits." short:"v"`
}

func main() { os.Exit(run()) }

func run() int {
	cli := CLI{}
	kong.Parse(&cli,
		kong.Name("codex-bridge"),
		kong.Description("Serve OpenAI Responses over HTTP by running one isolated codex exec per request."),
		kong.UsageOnError(),
	)

	level := slog.LevelInfo
	if cli.Verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runner, err := NewRunner(ctx, cli.Binary, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "codex-bridge:", err)
		return 2
	}
	defer runner.Close()

	server := &http.Server{
		Addr:              cli.Addr,
		Handler:           NewHandler(runner, log),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()

	log.Info("serving", "addr", cli.Addr, "binary", runner.Path, "account", "ChatGPT")
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "codex-bridge:", err)
		return 2
	}
	return 0
}
