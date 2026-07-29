// Package pipe is the stdin-to-stdout transport. It owns argv-adjacent
// concerns only: reading and writing the streams, event framing, and the exit
// code. Flag precedence, backend selection, translation and the dropped-field
// diff are core concerns it hands over and never implements.
package pipe

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/matthijn/dragoman/internal/base"
	"github.com/matthijn/dragoman/internal/core"
)

type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

// Flusher is the part of a writer that can push bytes out now. Streamed output
// is written and flushed per event: accumulating events and writing them at
// the end yields byte-identical output while defeating the whole purpose of
// the mode, so this is a correctness requirement, not a performance note.
type Flusher interface{ Flush() error }

// Report is what the caller asked to see beside the answer. Diagnostics go to
// stderr so that stdout stays the response a client can parse.
type Report struct {
	Verbose bool
	Timing  bool
}

// Run performs one translation and writes its result. It returns the exit
// code: zero when a response was returned and translated, one when the backend
// said no, and two when dragoman could not run.
func Run(ctx context.Context, catalog core.Catalog, opts core.Options, streams Streams, executor core.Executor, report Report) int {
	body, err := io.ReadAll(streams.In)
	if err != nil {
		return fail(streams.Err, core.Fail(core.ExitUsage, "reading input: %s", err))
	}

	// Wall time is the transport's to measure: it owns the invocation, and the
	// core stays free of a clock so that translation is a function from data
	// to data.
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
	// Drops are returned as data; the transport decides they go to stderr. A
	// drop that could not have changed the exchange is part of the diff rather
	// than news, so it is kept for -v, where the caller asked for all of it.
	if runErr == nil {
		reported := result.Drops
		if !report.Verbose {
			reported, _ = base.PartitionDrops(result.Drops)
		}
		if len(reported) > 0 {
			fmt.Fprintln(streams.Err, base.FormatDrops(reported))
		}
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

// OpenInput returns the reader for a request body, and whether it must be
// closed.
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

// writeEvent frames one event as a line of newline-delimited JSON and pushes
// it out. Re-framing this identical sequence as server-sent events is what the
// HTTP transport will do, so the framing choice never costs fidelity.
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

// writeTiming reports how long the round trip took. Time to the first event is
// the number that separates two models on the same prompt; total elapsed is
// the one that separates two backends.
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
