package pipe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/mdijkstra-oss/dragoman/internal/base"
	"github.com/mdijkstra-oss/dragoman/internal/core"
)

type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

type Flusher interface{ Flush() error }

type Report struct {
	Verbose bool
	Timing  bool
}

func Run(ctx context.Context, catalog core.Catalog, opts core.Options, streams Streams, executor core.Executor, report Report) int {
	body, err := io.ReadAll(streams.In)
	if err != nil {
		return fail(streams.Err, core.Fail(core.ExitUsage, "reading input: %s", err))
	}

	started := time.Now()
	firstEvent := time.Duration(0)
	sink := func(event base.Event) error {
		if firstEvent == 0 {
			firstEvent = time.Since(started)
		}
		return writeEvent(streams.Out, event)
	}
	result, runErr := core.Run(ctx, catalog, opts, body, executor, sink)
	elapsed := time.Since(started)

	if report.Timing {
		writeTiming(streams.Err, elapsed, firstEvent)
	}
	if report.Verbose {
		writeVerbose(streams.Err, result)
	}
	if runErr == nil && report.Verbose && len(result.Drops) > 0 {
		fmt.Fprintln(streams.Err, base.FormatDrops(result.Drops))
	}
	if runErr != nil {
		return fail(streams.Err, runErr)
	}

	switch {
	case opts.NoExecute:
		return writeBody(streams, result.Sent)
	case opts.Raw:
		return writeBody(streams, result.Received)
	case result.Response != nil:
		encoded, err := json.Marshal(result.Response)
		if err != nil {
			return fail(streams.Err, core.Fail(core.ExitUsage, "encoding response: %s", err))
		}
		return writeBody(streams, encoded)
	}
	return int(core.ExitOK)
}

func OpenInput(path string) (io.ReadCloser, error) {
	if path == "" || path == "-" {
		return io.NopCloser(os.Stdin), nil
	}
	return os.Open(path)
}

func OpenOutput(path string) (io.WriteCloser, error) {
	if path == "" || path == "-" {
		return nopCloser{os.Stdout}, nil
	}
	return os.Create(path)
}

func writeEvent(out io.Writer, event base.Event) error {
	encoded, err := base.EncodeEvent(event)
	if err != nil {
		return err
	}
	if _, err := out.Write(append(encoded, '\n')); err != nil {
		return err
	}
	if flusher, can := out.(Flusher); can {
		return flusher.Flush()
	}
	return nil
}

func writeBody(streams Streams, body []byte) int {
	if len(body) == 0 {
		return int(core.ExitOK)
	}
	if _, err := streams.Out.Write(append(body, '\n')); err != nil {
		return fail(streams.Err, core.Fail(core.ExitUsage, "writing output: %s", err))
	}
	if flusher, can := streams.Out.(Flusher); can {
		_ = flusher.Flush()
	}
	return int(core.ExitOK)
}

// Time to the first event separates two models on the same prompt; total elapsed
// separates two backends.
func writeTiming(err io.Writer, elapsed, firstEvent time.Duration) {
	if firstEvent > 0 {
		fmt.Fprintf(err, "elapsed %s, first event %s\n", round(elapsed), round(firstEvent))
		return
	}
	fmt.Fprintf(err, "elapsed %s\n", round(elapsed))
}

func round(d time.Duration) time.Duration { return d.Round(time.Millisecond) }

func writeVerbose(err io.Writer, result core.Result) {
	if len(result.Sent) > 0 {
		fmt.Fprintf(err, "translated request:\n%s\n", result.Sent)
	}
	if len(result.Received) > 0 {
		fmt.Fprintf(err, "backend response:\n%s\n", result.Received)
	}
}

func fail(err io.Writer, failure error) int {
	fmt.Fprintf(err, "dragoman: %s\n", failure)
	return int(core.CodeOf(failure))
}

type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
