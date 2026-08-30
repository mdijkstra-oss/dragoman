// Command claude-bridge serves the anthropic-messages wire format on localhost
// and answers every request by running the locally installed claude CLI, so a
// dragoman service pointed at it bills the account that signed in rather than
// an API key.
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
	Addr    string `help:"Address to listen on." default:"127.0.0.1:8082"`
	Binary  string `help:"The claude executable, a name on PATH or a path." default:"claude"`
	Verbose bool   `help:"Log every frame the CLI emits." short:"v"`
	Web     bool   `help:"Let the CLI use its own web search and fetch."`
}

func main() {
	os.Exit(run())
}

func run() int {
	cli := CLI{}
	kong.Parse(&cli,
		kong.Name("claude-bridge"),
		kong.Description("Serve anthropic-messages over HTTP by running the claude CLI.\n\n"+
			"One turn per request, no caller tools, streaming only. See cmd/claude-bridge/README.md."),
		kong.UsageOnError(),
	)

	level := slog.LevelInfo
	if cli.Verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	runner, err := NewRunner(ctx, cli.Binary)
	if err != nil {
		fmt.Fprintln(os.Stderr, "claude-bridge:", err)
		return 2
	}
	defer runner.Close()
	runner.Web = cli.Web

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

	log.Info("serving", "addr", cli.Addr, "binary", runner.Path, "account", runner.Account, "web", cli.Web)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, "claude-bridge:", err)
		return 2
	}
	return 0
}
