# dragoman

A stateless protocol-translating LLM proxy.

A request goes in written in `openai-responses`, gets executed against a provider
speaking another dialect, and comes back as `openai-responses`. The client never
learns which provider answered.

```sh
dragoman --backend anthropic --model claude-sonnet-5 < request.json
dragoman --backend gemini --model gemini-3.6-flash --stream < request.json
dragoman serve --addr 127.0.0.1:8080
```

One request in, one response out. No session, no stored conversation, no agent
loop, no tools executed on the caller's behalf. Everything needed to answer is
in the body that arrived, which is what makes the binary interchangeable with
the endpoint it stands in front of.

dragoman moves structure and never meaning. Values pass through untranslated,
prompts are never written, capabilities are never emulated.

## Dialects

The application speaks `openai-responses` and receives `openai-responses` back.
Five dialects are available as a backend:

| value | wire |
|---|---|
| `openai-responses` | `POST /responses` |
| `openai-completions` | `POST /chat/completions` — DeepSeek, OpenRouter, and every other compatible endpoint |
| `openai-chat-completions` | `POST /chat/completions` as OpenAI's own endpoint serves it |
| `anthropic-messages` | `POST /messages` |
| `gemini-interactions` | `POST /interactions` |

The operation path is what the dialect contributes; the base URL comes from the
service. The service table spells this `protocol` and the flag is
`--backend-protocol`; it is the same set of names.

A dialect is a wire shape, not a vendor. `openai-completions` names a body
format several companies serve, and `openai-chat-completions` exists because
OpenAI's chat endpoint is not quite that format: it spells the output cap
`max_completion_tokens` and reports usage while streaming only when asked with
`stream_options.include_usage`, where DeepSeek and OpenRouter use `max_tokens`
and report it unasked. Two wire shapes are two protocols; one name over both is
what forces a config key to paper over the difference.

## Services

A service is a protocol at a URL with a key in the environment. That is data
rather than code, so it is a file:

```yaml
openrouter:
  endpoint: https://openrouter.ai/api/v1
  protocol: openai-completions
  auth: OPENROUTER_API_KEY
```

| key | meaning |
|---|---|
| `endpoint` | base URL; the operation path belongs to the protocol |
| `protocol` | one of the five above |
| `auth` | **name of the environment variable** holding the key, never the key |
| `no_schema` | optional; this endpoint has no structured-output mode |

Three keys carry every service, and one endpoint sets the fourth:

> **Placement belongs to the protocol. Capability belongs to the service.**

Which field the output cap lands in, whether usage must be asked for while
streaming, whether to stream upstream at all — placement, every one, and the
protocol owns them. `no_schema` is the one thing that fails that test: DeepSeek
and OpenRouter both speak `openai-completions` and disagree about `json_schema`,
so no protocol can know it.

The shipped table:

| service | protocol | auth |
|---|---|---|
| `openai` | `openai-responses` | `OPENAI_API_KEY` |
| `openai-chat` | `openai-chat-completions` | `OPENAI_API_KEY` |
| `anthropic` | `anthropic-messages` | `ANTHROPIC_API_KEY` |
| `gemini` | `gemini-interactions` | `GEMINI_API_KEY` |
| `deepseek` | `openai-completions` | `DEEPSEEK_API_KEY` |
| `openrouter` | `openai-completions` | `OPENROUTER_API_KEY` |

`deepseek` carries `no_schema: true`; nothing else needs a fourth key. The table
ships embedded in the binary, and `--config` **replaces** it wholesale — there is
no merge and no Go map beside it, so the file in front of you is the whole
answer to what this instance serves. The consequence to accept is that a config
naming one service serves exactly that one; `dragoman config` prints the
embedded table for exactly that reason, to be copied and edited.

`--backend` on the pipe and `/responses/<service>` on the listener resolve the
same name from the same table, so a service added once is served by both. Two
tables would drift, and the one that drifted would be the one not under test.

A table naming a protocol dragoman does not speak, or a service missing
`endpoint` or `auth`, fails at load: exit `2`, dragoman could not run. **An unset
environment variable is not a load failure.** It fails when that service is
called, naming the variable, because an instance serving six providers has to
start with the one key its operator holds.

Nothing has to be in a table at all:

```sh
dragoman --backend-protocol gemini-interactions \
         --endpoint https://generativelanguage.googleapis.com/v1beta \
         --model gemini-3.6-flash < req.json
```

Anything the table does is expressible in flags, and a protocol supplies its own
default variable (`GEMINI_API_KEY` here), so an endpoint nobody anticipated is a
config edit or a command line rather than a patch.

Credentials come from the environment only, never from a flag and never from the
file. argv is visible in `ps`, in shell history, and in CI logs; a key written
into a config is a key in a repository, and this file is meant to be committed.
The header the key travels in is not configured either: `x-api-key`,
`Authorization: Bearer`, `x-goog-api-key` is placement, and placement is
structural.

## Model and effort

The **field** is structural, and dragoman maps it — each dialect keeps the model
and the reasoning level somewhere different:

| dialect | reasoning level lives at |
|---|---|
| `openai-responses` | `reasoning.effort` |
| `openai-completions` | `reasoning_effort` |
| `openai-chat-completions` | `reasoning_effort` |
| `anthropic-messages` | `output_config.effort` |
| `gemini-interactions` | `generation_config.thinking_level` |

The **value** is opaque. It passes through byte for byte: never normalised,
case-folded, validated against a set, or mapped onto a nearer neighbour.
Reasoning levels do not denote comparable strength across providers, so any
mapping dragoman invented would be a guess wearing the costume of a conversion.

> dragoman owns the set of dialects. Providers own the set of models and levels.

Precedence is the `--model`/`--effort`/`--max-tokens` flag, then the value in the
body, then nothing. **Absent stays absent** — filling in a level nobody asked for
silently overrides a provider default and changes both cost and behaviour.

`anthropic-messages` is the only wire format that refuses a request without an
output cap. When neither flag nor body supplies one, that protocol itself
supplies 20000; no other backend ever receives an invented value. Where the cap
lands is the protocol's business too — `max_tokens`, `max_completion_tokens`, or
under `generation_config` — because a service entry able to relocate a field
would be a config file making a structural decision.

## Streaming

`stream` is an **output** setting. It decides what the caller receives, not how
dragoman talks to the backend.

- default — one `openai-responses` body on stdout, written once, when the answer
  is complete.
- `stream: true` in the body, or `--stream` — the `openai-responses` event
  sequence, one event per line, written and flushed per event.

What is emitted is the complete Responses lifecycle, not the deltas alone, so a
stock SDK reconstructs the answer from that sequence the way it always has.
Framing belongs to the transport and never costs fidelity: the pipe writes the
sequence as newline-delimited JSON, the listener writes the identical sequence
as SSE.

`anthropic-messages` is always streamed from the backend, whatever the caller
asked for — its own SDKs require streaming at the cap that protocol supplies,
and the caller cannot tell.

The two axes are independent, and one invariant keeps them honest:

> **Folding a translated event stream equals the translated buffered response**,
> for every fixture and every backend.

## Serving

The pipe answers one request per process, which most clients cannot use: an SDK
points at a base URL, and that is the whole of its integration surface.

```sh
dragoman serve --addr 127.0.0.1:8080 --config ./dragoman.yaml
```

| route | purpose |
|---|---|
| `POST /responses/<service>` | the path segment is a key in the service table; the body is an unmodified `openai-responses` request |
| `GET /services` | the table as JSON — name, protocol, endpoint, and the variable each needs. Never a key, and never whether one is set |
| `GET /health` | liveness only |

Nothing about a request is configured out of band. `model`, `reasoning.effort`,
`max_output_tokens`, `stream`, `tools`, `tool_choice`, `text.format` and
`instructions` already travel in the body, so the surface is one path segment
rather than a flag set per route. `--config` and `--strict` are the only other
things a listener settles, and both apply to every request it serves. An
application changes provider by changing a path segment.

`stream` is decided by the body exactly as on the pipe, and only the framing
changes: a buffered request gets `application/json` and one Responses body, a
streaming one gets `text/event-stream` carrying the same event sequence, flushed
per event.

**No client authentication**, and the default bind is `127.0.0.1`. A shared
bearer token in front of a process holding provider keys is authentication
theatre; an instance that needs to be reachable belongs behind something that
does authentication as its actual job.

The exit codes below become status codes:

| code | HTTP | when |
|---|---|---|
| `2` | `400` | unparseable body, a required field with no source, a drop under `--strict` |
| `2` | `404` | unknown service |
| `2` | `500` | the service's `auth` variable is unset |
| `1` | the backend's own status | the backend refused |
| `0` | `200` | translated |

A missing credential is `500` and not `400`: the caller did nothing wrong, the
instance is misconfigured, and a `4xx` there would send an SDK into a retry loop
over an operator error. A refusal keeps the backend's own status, because a rate
limit carries retry semantics the SDK already implements; a provider error that
arrived on a `200` becomes `502`. Failures are `openai-responses` error objects
— a client that knows only this format has to be able to read them too. Once the
first event of a stream is written the status line is already sent, so a
mid-stream failure ends the stream as an event.

## Provider state

Every provider returns something it wants handed back unaltered, and each binds
it to the item it arrived on. dragoman carries it in the item's
`encrypted_content` and puts it back where it belongs — a signature on an
Anthropic thinking block, a Gemini thought signature (which may ride on the call
itself), DeepSeek's reasoning content, OpenAI's encrypted reasoning.

**A conversation lives on one backend for its whole life.** Histories are not
interchangeable between providers and never could be.

Because dragoman is stateless, that state survives a turn boundary only in the
client's hands: echo the response's output items back in the next request's
input. An application that reconstructs history from rendered text will drop it.

## Dropped fields

A drop is any key present in a body that no typed field consumed — computed,
never hand-maintained. Entries carry a path, not a name
(`input[3].content[1].signature`), because skip-and-continue makes this the
caller's only signal.

- **unsupported** — the target dialect has no equivalent and never will.
- **unimplemented** — dragoman could map it and does not yet.

By default the response is returned and drops go to stderr — to the server log
on the listener, never into the answer. `--strict` makes any drop an error: exit
`2`, nothing sent. Silence is not an option — a dropped field produces a
confident, well-formed, wrong answer.

A schema sent to a service marked `no_schema` is the one drop that is the
designed outcome: `text.format` goes, the drop is recorded, and the caller gets a
`200` and prose. Prose where JSON was expected reads as a model quirk the caller
adapts to; a refusal dragoman invented on the provider's behalf reads as
dragoman being broken. `--strict` promotes it like any other.

## Diagnostics

| flag | effect |
|---|---|
| `--no-execute` | translate the request, print what would have been sent, make no call |
| `--raw` | return the backend's untranslated response |
| `-v` | the translation and its dropped fields to stderr |
| `--timing` | elapsed round trip, and time to the first event, to stderr |

`--raw` returns exactly what the backend sent. For a protocol that streams
upstream regardless of the output mode — `anthropic-messages` — that is the
event transcript rather than a body, which is the point: it is what a mapper bug
would have been read from.

## Exit codes

| code | meaning |
|---|---|
| `0` | a response was returned and translated |
| `1` | the backend said no: HTTP 4xx/5xx, or a provider error body |
| `2` | dragoman could not run: unparseable input, unknown dialect, a service table that will not load, unknown service, missing key, bad flags, a required field with no source, or a drop under `--strict` |

`2` is reserved for "the tool could not run", which is the only thing that makes
it trustworthy in a hook or a CI step. The same three classes are what the
listener maps to status codes.

## Layout

Dependencies flow one way. The base imports nothing; dialect packages import the
base only, never each other; transports import the core; `cmd/` wires them
together.

```
internal/base/                 the openai-responses format — types, the event
                               grammar, the emitter, the fold, the drop diff
internal/dialect/              the shape every wire format implements
internal/dialect/*/            one package per dialect
internal/config/               the service table — YAML in, backends out, and
                               the embedded default
internal/core/                 flag precedence, backend selection, the pipeline
internal/catalog/              the protocols dragoman speaks, and the table
                               built out of them
internal/transport/pipe/       stdin to stdout
internal/transport/serve/      HTTP in, one body or an SSE stream out
cmd/dragoman/                  argv, streams, exit codes — and nothing else
```

The core takes a request and returns a response or an event sequence, and never
touches `os.Stdout`. That is why the listener is an addition and not a refactor:
it reads a body, hands the core a sink that writes SSE, and maps the failure it
gets back to a status. Framing and that mapping are the whole of what it owns.

## Building and testing

```sh
go build -o bin/dragoman ./cmd/dragoman
go test ./...
```

The suite runs offline against responses recorded from four live backends in
`testdata/fixtures/`, over a real `httptest` server for the listener. No key and
no network are needed.

`openai-chat-completions` has no recorded fixture, so it is covered by
translation assertions rather than by a round trip. That is the honest extent of
it until one is recorded.
