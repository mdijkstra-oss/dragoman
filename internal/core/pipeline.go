package core

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/mdijkstra-oss/dragoman/internal/base"
	"github.com/mdijkstra-oss/dragoman/internal/dialect"
)

type Catalog struct {
	Dialects  map[string]dialect.Dialect
	Presets   map[string]base.Backend
	Executors Executors
}

type Executors map[string]Executor

type Sink func(base.Event) error

type Result struct {
	Response *base.Response

	// A streamed answer leaves no response body behind, and the counts are the
	// same either way.
	Usage *base.Usage

	Drops    []base.Drop
	Sent     json.RawMessage
	Received json.RawMessage
}

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
	if _, known := catalog.Dialects[backend.Dialect]; !known {
		return base.Backend{}, Fail(ExitUsage, "unknown backend protocol %q", backend.Dialect)
	}
	if backend.Endpoint == "" {
		return base.Backend{}, Fail(ExitUsage, "no endpoint for %q: pass --endpoint", backend.Dialect)
	}
	// A key belongs to the host it was issued for. Left to be inherited, an
	// endpoint named on the command line would be handed whichever key the
	// preset or the protocol happened to supply.
	if opts.Endpoint != "" && opts.AuthEnv == "" {
		return base.Backend{}, Fail(ExitUsage,
			"--endpoint %s needs --auth-env naming the variable that holds that endpoint's key", opts.Endpoint)
	}
	if opts.AuthEnv != "" {
		backend.KeyEnv = opts.AuthEnv
	}
	if backend.KeyEnv == "" {
		return base.Backend{}, Fail(ExitUsage,
			"no credential for %q: pass --auth-env naming the variable that holds the key", backend.Endpoint)
	}
	if backend.Name == "" {
		backend.Name = backend.Dialect
	}
	return backend, nil
}

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
	upstream := WantsEvents(req, opts) || spoken.StreamUpstream

	encoded, err := spoken.Encode(req, backend, upstream)
	if err != nil {
		return req, backend, spoken, dialect.Encoded{}, Fail(ExitUsage, "%s", err)
	}
	return req, backend, spoken, encoded, nil
}

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

	// A backend naming no variable holds its own credential.
	key := ""
	if backend.KeyEnv != "" {
		key = os.Getenv(backend.KeyEnv)
		if key == "" {
			return result, Fail(ExitUsage, "missing credential: set %s: %w", backend.KeyEnv, ErrMissingCredential)
		}
	}

	reach, err := resolveExecutor(catalog, backend, executor)
	if err != nil {
		return result, err
	}

	wantsEvents := WantsEvents(req, opts)
	upstream := wantsEvents || spoken.StreamUpstream

	call := Call{
		URL:     targetOf(spoken, backend),
		Headers: spoken.Headers(key),
		Body:    encoded.Body,
		Stream:  upstream,
	}

	round, err := exchange(ctx, reach, spoken, call, wantsEvents && !opts.Raw, sink)
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
	folded = base.AsFormattedText(folded, req)
	result.Usage = folded.Usage
	result.Drops = base.MergeDrops(result.Drops, round.Drops)
	if opts.Strict && len(result.Drops) > 0 {
		return result, Fail(ExitUsage, "strict: %d field(s) dropped:\n%s",
			len(result.Drops), base.FormatDrops(result.Drops))
	}
	if !wantsEvents {
		result.Response = &folded
	}
	// A provider error can arrive inside a 200.
	if folded.Status == base.StatusFailed || folded.Error != nil {
		return result, Fail(ExitBackend, "backend reported an error: %s", errorText(folded))
	}
	return result, nil
}

func resolveExecutor(catalog Catalog, backend base.Backend, supplied Executor) (Executor, error) {
	if supplied != nil {
		return supplied, nil
	}
	scheme := base.SchemeOf(backend.Endpoint)
	chosen, known := catalog.Executors[scheme]
	if !known {
		return nil, Fail(ExitUsage, "no executor for endpoint scheme %q in %q", scheme, backend.Endpoint)
	}
	return chosen, nil
}

func targetOf(spoken dialect.Dialect, backend base.Backend) string {
	return dialect.Endpoint(spoken, backend)
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
