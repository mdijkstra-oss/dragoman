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

dragoman moves structure and never meaning. Values pass through untranslated and
capabilities are never emulated, with one exception a service opts into by name:
[`schema_as_prompt`](#services), for an endpoint whose JSON mode has to be told
the shape in words.

## Dialects

The application speaks `openai-responses` and receives `openai-responses` back.
Four dialects are available as a backend:

| value | body format |
|---|---|
| `openai-responses` | the same format dragoman accepts, forwarded to a provider that serves it |
| `openai-completions` | chat completions, as OpenAI, DeepSeek, OpenRouter and every other compatible endpoint serve it |
| `anthropic-messages` | Anthropic's messages format |
| `gemini-interactions` | Google's Gemini format |

The service table spells this `protocol` and the flag is `--backend-protocol`;
it is the same set of names.

A dialect is a wire shape, not a vendor. `openai-completions` names a body
format several companies serve and one no company owns, which is why it is the
only protocol with no default credential: an endpoint reached by flags has to
name its own with `--auth-env`.

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
| `protocol` | one of the four above |
| `auth` | **name of the environment variable** holding the key, never the key |
| `max_output_tokens_as` | optional; this endpoint spells the output cap `max_tokens` |
| `schema_as_prompt` | optional; this endpoint has a JSON mode and no schema mode |

Three keys carry every service, and one endpoint sets both of the rest:

> **Placement belongs to the protocol. Capability belongs to the service.**

How usage is reported, whether to stream upstream at all, where every field
lands — placement, and the protocol owns them, because they follow from the wire
format. Two keys fail that test for the same reason: DeepSeek and OpenRouter
both speak `openai-completions` and disagree with each other, about `json_schema`
and about how the output cap is spelled. No protocol can know either.

Omitting both is the default, which is legal on every protocol — most services
name neither. Writing one is what carries a claim, and it is a claim only
`openai-completions` can honour, so it fails at load anywhere else: exit `2`,
naming the service and the protocol it named.

```console
$ dragoman serve --config ./bad.yaml
dragoman: config: service "anthropic" sets schema_as_prompt on protocol "anthropic-messages" (only openai-completions has a JSON mode to fall back to)
```

A `json_schema` arriving for a service that did set it becomes two things:
`response_format` of `{"type": "json_object"}`, and the schema itself written
into `instructions` behind whatever prompt the caller sent. Both halves are
required — the mode alone yields JSON of any shape, and the prompt alone yields
prose that happens to contain some.

> [!IMPORTANT]
> A schema in a prompt is a request, not a constraint, and `strict` is gone with
> the field that carried it. The drop is recorded on every such request so the
> caller knows the enforcement it asked for is not there.

The shipped table:

| service | protocol | auth |
|---|---|---|
| `openai` | `openai-responses` | `OPENAI_API_KEY` |
| `openai-chat` | `openai-completions` | `OPENAI_API_KEY` |
| `anthropic` | `anthropic-messages` | `ANTHROPIC_API_KEY` |
| `gemini` | `gemini-interactions` | `GEMINI_API_KEY` |
| `deepseek` | `openai-completions` | `DEEPSEEK_API_KEY` |
| `openrouter` | `openai-completions` | `OPENROUTER_API_KEY` |

`deepseek` carries both optional keys; nothing else needs either. The table
ships embedded in the binary, and `--config` **replaces** it wholesale — there is
no merge and no Go map beside it, so the file in front of you is the whole
answer to what this instance serves. The consequence to accept is that a config
naming one service serves exactly that one; `dragoman config` prints the
embedded table for exactly that reason, to be copied and edited.

`--backend` on the pipe and the first segment of `model` on the listener resolve
the same name from the same table, so a service added once is served by both.
Two tables would drift, and the one that drifted would be the one not under
test.

A table naming a protocol dragoman does not speak, or a service missing
`endpoint` or `auth`, fails at load: exit `2`, dragoman could not run. **An unset
environment variable is not a load failure.** It fails when that service is
called, naming the variable, because an instance serving six providers has to
start with the one key its operator holds. `auth` is required of every service
dragoman reaches over the network; the one kind that holds its own credential is
[the local CLI](#the-local-cli).

Nothing has to be in a table at all:

```sh
dragoman --backend-protocol gemini-interactions \
         --endpoint https://generativelanguage.googleapis.com/v1beta \
         --model gemini-3.6-flash < req.json
```

Most protocols supply their own default variable (`GEMINI_API_KEY` here), so an
endpoint nobody anticipated is a config edit or a command line rather than a
patch. `openai-completions` is the exception at both ends: no company owns it, so
there is no variable to default to and `--auth-env` names one, and the two
optional keys describe an endpoint rather than a wire format, so they are the one
thing a command line cannot restate. An endpoint that needs either needs a table
entry.

Credentials come from the environment only, never from a flag and never from the
file. argv is visible in `ps`, in shell history, and in CI logs; a key written
into a config is a key in a repository, and this file is meant to be committed.
The header the key travels in is not configured either: `x-api-key`,
`Authorization: Bearer`, `x-goog-api-key` is placement, and placement is
structural.

## The local CLI

One service is a process rather than a URL. `claude-cli` spawns a locally
installed Claude Code, which authenticates itself, so a developer with a
subscription and no API key can route a request through it:

```sh
dragoman --backend claude-cli --model sonnet < request.json
```

It is a row of the same table as every other service, so the listener serves it
too — a request naming `claude-cli/sonnet` as its model reaches the same backend
the pipe does, and `GET /services` reports it with an empty `auth`.

`--backend claude-cli` is sugar for `--backend-protocol anthropic-messages
--endpoint exec:claude`, and the `exec:` scheme is what selects a process. The
scheme also carries what such a backend cannot do, so naming the binary in
flags behaves exactly as the table entry does.

The CLI is an agent by default — it reads `CLAUDE.md`, runs hooks, resolves
skills and plugins, persists a session and executes tools. None of that was
asked for by a caller who sent a Responses body, so dragoman fixes the flags
that turn it back into a model endpoint and does not make them configurable.

**What it cannot do is dropped, loudly.** Reaching an agent harness costs two
capabilities outright, and limits a third:

| dropped | why |
|---|---|
| `tools[n]`, `tool_choice`, `parallel_tool_calls` | the CLI resolves its own tools and has no channel to hand a call back |
| `max_output_tokens` | it has no flag for an output cap |

A second **user** message is not a drop but a refusal, exit `2` naming both
paths. The CLI runs a turn per user message, so sending one would run an extra
turn whose answer nobody asked for, and dropping one would change the
conversation. A tool result counts as a user turn.

> The envelope is: any amount of context, exactly one question, no tools the
> caller owns. A one-shot client works; a chat client does not.

Two preflight checks run before the call, both exit `2`: the binary is on
`PATH`, and `claude auth status` reports a session. Neither costs a token.

## Model and effort

The **field** is structural, and dragoman maps it — each dialect keeps the model
and the reasoning level somewhere different:

| dialect | reasoning level lives at |
|---|---|
| `openai-responses` | `reasoning.effort` |
| `openai-completions` | `reasoning_effort` |
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
| `POST /responses` | an `openai-responses` request whose `model` names its service first: `openai/gpt-5.5` |
| `GET /services` | the table as JSON — name, protocol, endpoint, and the variable each needs. Never a key, and never whether one is set |
| `GET /health` | liveness only |

**The service is the first segment of `model`, split on the first `/` only.**
`openai/gpt-5.5` reaches `openai` with `gpt-5.5` on the forwarded body, and
`openrouter/anthropic/claude-3` reaches `openrouter` with `anthropic/claude-3`,
because a provider whose own identifiers are paths has to stay addressable. A
model with no service, or one naming a service this instance does not serve, is
a `404` listing what it does. Prefixing the model is how OpenRouter spells the
same idea, so a client already writing one needs no field it does not have.

Nothing about a request is configured out of band, and there is nothing in the
URL to configure: `model`, `reasoning.effort`, `max_output_tokens`, `stream`,
`tools`, `tool_choice`, `text.format` and `instructions` all travel in the body,
so one base URL is a client's whole integration surface. `--config` and
`--strict` are the only things a listener settles, and both apply to every
request it serves. An application changes provider by changing a model name.

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
| `2` | `404` | a `model` naming no service this instance serves |
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

## Accounting

The listener logs JSON, and a request that worked is one line of it:

```json
{"time":"2026-07-31T12:04:11Z","level":"INFO","msg":"request served","service":"anthropic","model":"claude-4-6","status":200,"duration_ms":842,"input_tokens":516,"cached_tokens":0,"output_tokens":160,"reasoning_tokens":34,"total_tokens":676}
```

A translating proxy is the only thing that knows both which service answered and
what the answer cost, so the record belongs here. The counts are the normalised
ones — the same key means the same thing whichever provider answered — written
whether the caller streamed or not, and at zero rather than absent when a backend
reported none, because a line whose key set depends on who answered cannot be
summed. `duration_ms` is dragoman's exchange with the backend, never a caller's
view of its own request. A failed request writes the failure line and not this
one.

`LOG_REQUEST_HEADERS` copies request headers onto that line, which is how a
caller in front of dragoman puts its own identity beside the cost — a request ID
shared with its own log, a session, whichever agent it resolved:

```sh
LOG_REQUEST_HEADERS=X-Request-ID,X-Session-ID dragoman serve
```

**An allowlist, never a passthrough.** A request carrying fifty headers produces
entries for the names configured here or nothing; unset records nothing at all.
A name the request omits is left out rather than written empty. At most sixteen
names, each canonicalizing to `X-*`, none containing a credential-like term
(`auth`, `api-key`, `secret`, `password` among them) — a violation fails at
load: exit `2`, naming the entry. An operator who can name any header to log can
name the one carrying a key, and a key in a log is a key in every archive of it.

A recorded value is a label. Dragoman performs no authentication, so a header
naming a subject grants nothing, reaches no routing decision, and selects no
credential.

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
- **inert** — the target has no equivalent and needs none: the field could not
  have changed the answer. A schema's name is a label; `strict: true` against a
  dialect that enforces a schema anyway is asking for what the request gets.

By default the response is returned and the drops that changed the exchange go
to stderr — to the server log as a warning on the listener, never into the
answer. Silence is not an option — a dropped field produces a confident,
well-formed, wrong answer. An inert drop is not that: it is kept for `-v` on the
pipe and for the debug log on the listener, because a warning repeated on every
request of a kind is how the warning that matters gets skimmed past.

`--strict` makes any drop an error, inert included: exit `2`, nothing sent. The
flag is for the caller who wants to hear about every difference.

A schema sent to a service marked `schema_as_prompt` is the one drop that is the
designed outcome: `text.format` goes, the drop is recorded, and the shape reaches
the model as prose instead. What the caller loses is enforcement, which is worth
saying on every request — an answer that is valid JSON of the wrong shape is a
parse failure with nothing in the exchange to explain it. `--strict` promotes it
like any other.

## Diagnostics

| flag | effect |
|---|---|
| `--no-execute` | translate the request, print what would have been sent, make no call |
| `--raw` | return the backend's untranslated response |
| `-v` | the translation and every dropped field, inert ones included, to stderr |
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
| `2` | dragoman could not run: unparseable input, unknown dialect, a service table that will not load, a log-header allowlist that will not, unknown service, missing key, bad flags, a required field with no source, or a drop under `--strict` |

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
internal/core/                 flag precedence, backend selection, the pipeline,
                               and the executors that reach a backend over HTTP
                               or by spawning a local CLI
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

OpenAI's own chat endpoint has no recorded fixture, so it is covered by
translation assertions rather than by a round trip. That is the honest extent of
it until one is recorded.
