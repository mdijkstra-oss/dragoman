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
	Dialects  map[string]dialect.Dialect
	Presets   map[string]base.Backend
	Executors Executors
}

// Executors resolves an endpoint's scheme onto the thing that reaches it.
// Selection is a dispatch table rather than a branch on the endpoint, so a
// backend that is a process rather than a URL is a table entry.
type Executors map[string]Executor

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
	backend.KeyEnv = keyEnvFor(backend, spoken)
	backend = withSchemeLimits(backend)
	if backend.Name == "" {
		backend.Name = backend.Dialect
	}
	return backend, nil
}

// withSchemeLimits adds what follows from how a backend is reached rather than
// from which endpoint it is. A locally installed agent CLI resolves its own
// tools, takes no output cap and runs a turn per user message. Those are facts
// about spawning such a binary, so the table entry that names one and the flags
// that name the same one carry them identically — and getting them wrong is the
// confident, well-formed, wrong answer this design exists to prevent, which is
// too much to leave to whether the table was used.
func withSchemeLimits(backend base.Backend) base.Backend {
	if !base.IsSpawned(backend.Endpoint) {
		return backend
	}
	reached := backend
	reached.NoTools = true
	reached.NoOutputCap = true
	reached.OneTurnPerRequest = true
	return reached
}

// keyEnvFor names the variable a backend authenticates with, or the empty
// string when it holds its own credential. A locally installed CLI does: it was
// signed in once, interactively, and there is no key to find. The scheme
// decides rather than the table, so that a backend named in flags and the same
// backend named in the table authenticate identically.
func keyEnvFor(backend base.Backend, spoken dialect.Dialect) string {
	if base.IsSpawned(backend.Endpoint) {
		return ""
	}
	if backend.KeyEnv != "" {
		return backend.KeyEnv
	}
	return spoken.KeyEnv
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
	upstream := WantsEvents(req, opts) || spoken.StreamUpstream

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

	// A backend that names no variable holds its own credential, and there is
	// nothing to look up or to refuse it for.
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

	// --raw returns the backend's own bytes, so the translated event sequence
	// is not also written.
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

// resolveExecutor picks how the backend is reached. A supplied executor wins,
// so that a recorded exchange can be replayed without a network or a
// subprocess; otherwise the endpoint's scheme selects one from the table.
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

// targetOf names where the backend is. A path belongs to an HTTP operation and
// a process is reached by name, so the scheme decides whether the dialect's
// path is appended to the endpoint.
func targetOf(spoken dialect.Dialect, backend base.Backend) string {
	if base.IsSpawned(backend.Endpoint) {
		return backend.Endpoint
	}
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
