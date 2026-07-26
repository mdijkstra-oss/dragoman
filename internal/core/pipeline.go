package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/matthijn/dragoman/internal/base"
	"github.com/matthijn/dragoman/internal/dialect"
)

// Catalog is the set of dialects dragoman knows and the presets that name
// backends built out of them. Presets are a convenience and must stay one:
// anything a preset does has to be expressible in flags, so that an endpoint
// nobody anticipated is a command line rather than a patch.
type Catalog struct {
	Dialects map[string]dialect.Dialect
	Presets  map[string]base.Backend
}

// Sink receives each event as it is produced. The core yields events and never
// decides when they reach a file descriptor.
type Sink func(base.Event) error

type Result struct {
	Response *base.Response
	Drops    []base.Drop
	Sent     json.RawMessage
	Received json.RawMessage
}

// SelectBackend resolves a backend from a preset and the flags that override
// it. A backend is a dialect at a URL with a key in the environment.
func SelectBackend(catalog Catalog, opts Options) (base.Backend, error) {
	backend := base.Backend{}
	if opts.Preset != "" {
		preset, known := catalog.Presets[opts.Preset]
		if !known {
			return base.Backend{}, Fail(ExitUsage, "unknown backend preset %q", opts.Preset)
		}
		backend = preset
	}
	if opts.BackendProtocol != "" {
		backend.Dialect = opts.BackendProtocol
	}
	if opts.Endpoint != "" {
		backend.Endpoint = opts.Endpoint
	}
	if backend.Dialect == "" {
		return base.Backend{}, Fail(ExitUsage, "no backend: pass --backend or --backend-protocol")
	}
	spoken, known := catalog.Dialects[backend.Dialect]
	if !known {
		return base.Backend{}, Fail(ExitUsage, "unknown backend protocol %q", backend.Dialect)
	}
	if backend.Endpoint == "" {
		return base.Backend{}, Fail(ExitUsage, "no endpoint for %q: pass --endpoint", backend.Dialect)
	}
	if backend.KeyEnv == "" {
		backend.KeyEnv = spoken.KeyEnv
	}
	if backend.Name == "" {
		backend.Name = backend.Dialect
	}
	return backend, nil
}

// Translate builds the backend request out of the base request and reports
// what could not be carried into it. This is the whole pipeline minus the
// network.
func Translate(catalog Catalog, opts Options, body []byte) (base.Request, base.Backend, dialect.Dialect, dialect.Encoded, error) {
	req, err := base.ParseRequest(body)
	if err != nil {
		return base.Request{}, base.Backend{}, dialect.Dialect{}, dialect.Encoded{}, Fail(ExitUsage, "%s", err)
	}
	backend, err := SelectBackend(catalog, opts)
	if err != nil {
		return base.Request{}, base.Backend{}, dialect.Dialect{}, dialect.Encoded{}, err
	}
	spoken := catalog.Dialects[backend.Dialect]

	req = Resolve(req, opts)
	upstream := WantsEvents(req, opts) || backend.Quirks.StreamUpstream

	encoded, err := spoken.Encode(req, backend, upstream)
	if err != nil {
		return req, backend, spoken, dialect.Encoded{}, Fail(ExitUsage, "%s", err)
	}
	encoded.Drops = base.Classify(encoded.Drops, spoken.DropTable)
	return req, backend, spoken, encoded, nil
}

// Run translates, executes, and decodes. When the caller asked for the event
// sequence, each event is handed to sink as it is produced; otherwise the
// folded response is returned.
func Run(ctx context.Context, catalog Catalog, opts Options, body []byte, executor Executor, sink Sink) (Result, error) {
	req, backend, spoken, encoded, err := Translate(catalog, opts, body)
	if err != nil {
		return Result{}, err
	}
	result := Result{Drops: encoded.Drops, Sent: encoded.Body}

	if opts.Strict && len(result.Drops) > 0 {
		return result, Fail(ExitUsage, "strict: %d field(s) would be dropped:\n%s",
			len(result.Drops), base.FormatDrops(result.Drops))
	}
	if opts.NoExecute {
		return result, nil
	}

	key := os.Getenv(backend.KeyEnv)
	if key == "" {
		return result, Fail(ExitUsage, "missing credential: set %s", backend.KeyEnv)
	}

	wantsEvents := WantsEvents(req, opts)
	upstream := wantsEvents || backend.Quirks.StreamUpstream

	call := Call{
		URL:     dialect.Endpoint(spoken, backend),
		Headers: spoken.Headers(key),
		Body:    encoded.Body,
		Stream:  upstream,
	}

	// --raw returns the backend's own bytes, so the translated event sequence
	// is not also written.
	round, err := exchange(ctx, executor, spoken, call, wantsEvents && !opts.Raw, sink)
	result.Received = round.Received
	if err != nil {
		return result, err
	}

	if opts.Raw {
		return result, nil
	}
	folded, err := base.Fold(round.Events)
	if err != nil {
		return result, Fail(ExitUsage, "%s", err)
	}
	result.Drops = base.MergeDrops(result.Drops, base.Classify(round.Drops, spoken.DropTable))
	if opts.Strict && len(result.Drops) > 0 {
		return result, Fail(ExitUsage, "strict: %d field(s) dropped:\n%s",
			len(result.Drops), base.FormatDrops(result.Drops))
	}
	if !wantsEvents {
		result.Response = &folded
	}
	// The backend said no in the body rather than in the status line. That is
	// still the backend saying no, and a CI step gating on the exit code has
	// to see it.
	if folded.Status == base.StatusFailed || folded.Error != nil {
		return result, Fail(ExitBackend, "backend reported an error: %s", errorText(folded))
	}
	return result, nil
}

func errorText(resp base.Response) string {
	if resp.Error == nil {
		return "no detail"
	}
	if resp.Error.Code == "" {
		return resp.Error.Message
	}
	return resp.Error.Code + ": " + resp.Error.Message
}

// exchange performs the call and decodes it, streaming events to the sink when
// the caller wants them. It returns the whole event sequence so the buffered
// path can fold it and the streaming path can still be checked.
func exchange(
	ctx context.Context,
	executor Executor,
	spoken dialect.Dialect,
	call Call,
	wantsEvents bool,
	sink Sink,
) (exchanged, error) {
	if !call.Stream {
		body, err := executor.Do(ctx, call)
		if err != nil {
			return exchanged{Received: body}, err
		}
		events, drops, decodeErr := dialect.DecodeBody(spoken, body)
		if decodeErr != nil {
			return exchanged{Received: body}, Fail(ExitUsage, "%s", decodeErr)
		}
		if err := drain(events, wantsEvents, sink); err != nil {
			return exchanged{Received: body}, err
		}
		return exchanged{Events: events, Received: body, Drops: drops}, nil
	}

	result := exchanged{}
	decoder := dialect.Decoder(spoken, func(drops []base.Drop) {
		result.Drops = append(result.Drops, drops...)
	})
	onFrame := func(frame base.Frame) error {
		result.Received = append(append(result.Received, frame.Data...), '\n')
		produced, err := decoder.Step(frame)
		if err != nil {
			return Fail(ExitUsage, "%s", err)
		}
		result.Events = append(result.Events, produced...)
		return drain(produced, wantsEvents, sink)
	}
	if err := executor.Stream(ctx, call, onFrame); err != nil {
		return result, err
	}
	flushed, err := decoder.Flush()
	if err != nil {
		return result, Fail(ExitUsage, "%s", err)
	}
	result.Events = append(result.Events, flushed...)
	if err := drain(flushed, wantsEvents, sink); err != nil {
		return result, err
	}
	return result, nil
}

// exchanged is what one round trip produced.
type exchanged struct {
	Events   []base.Event
	Received json.RawMessage
	Drops    []base.Drop
}

func drain(events []base.Event, wantsEvents bool, sink Sink) error {
	if !wantsEvents || sink == nil {
		return nil
	}
	for _, event := range events {
		if err := sink(event); err != nil {
			return fmt.Errorf("emit: %w", err)
		}
	}
	return nil
}
