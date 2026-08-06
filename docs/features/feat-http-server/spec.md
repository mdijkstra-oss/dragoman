---
slug: http-server
type: feat
status: draft
created: 2026-07-27
---

------

# Feature: HTTP server and the service table

## Purpose

dragoman translates one request per process today. Every client that wants that translation
has to be able to spawn a binary and hold a pipe, which most cannot: an OpenAI SDK points at
a base URL, and that is the only integration surface it has.

So the listener is not a convenience wrapper around the CLI. It is the surface the product
was for. An application sets `base_url` to a dragoman address, keeps its SDK, keeps its
Responses bodies, and changes provider by changing a path segment.

The second half is the same idea applied to backends. The set of endpoints anyone wants to
reach is not knowable in advance, and today it lives in a Go map — so reaching a new one
means a patch and a release. It becomes a file:

```yaml
openrouter:
  endpoint: https://openrouter.ai/api/v1
  protocol: openai-completions
  auth: OPENROUTER_API_KEY
```

Three keys, and there is no fourth. A backend was always a protocol at a URL with a key in
the environment; writing that as data rather than as Go is what makes an endpoint nobody
anticipated a config edit rather than a fork.

## Scope

### One table, and it is the config

**The service table replaces `catalog.Presets()`.** There is no Go map of backends beside
the YAML and no merge between them. The default table ships as a file embedded in the
binary, and `--config` replaces it wholesale.

This is not a server-only feature. `--backend` on the CLI and `/<service>/responses` on the
listener resolve the same name from the same table, so adding `groq:` to a config gives both
at once. Two tables would drift, and the one that drifted would be the one not being tested.

Replace rather than merge, because a config that owns the table can be read: the file in
front of you is the whole answer to "what does this instance serve". A merge means the
answer is the file plus a Go map the reader does not have open, and the failure mode is a
service resolving to an entry the operator did not write.

The consequence to accept: a config listing only `groq:` serves only `groq`. The embedded
default is the thing to copy and edit, and `dragoman config` prints it for exactly that.

### A service is three keys

| key | meaning |
|---|---|
| `endpoint` | base URL; the operation path belongs to the protocol |
| `protocol` | one of the five names below |
| `auth` | **name of the environment variable** holding the key, never the key |
| `no_schema` | optional; this endpoint has no structured-output mode |

Three keys carry every service, and one endpoint sets the fourth. The dividing line is
**placement versus capability**: *where* a field goes is a property of the wire format and
belongs to the protocol, while *whether an endpoint can do a thing at all* is a property of
that endpoint and nothing else knows it.

Everything an earlier draft wanted here failed that test and moved out — which field the
output cap lands in, whether usage must be requested while streaming, whether to stream
upstream are all placement. `no_schema` passes it: DeepSeek and OpenRouter both speak
`openai-completions` and OpenRouter supports `json_schema`, so the protocol cannot know.

`auth` names a variable because credentials come from the environment only. A key in a
config file is a key in a repository, and this file is meant to be committed — that is the
point of moving the table into one. The **header** that key travels in is likewise not
configured: `anthropic-messages` sends `x-api-key`, the OpenAI protocols send
`Authorization: Bearer`, Gemini its own. That is placement, and placement is structural.

An unknown `protocol` fails at load, naming the value and listing the five. A missing
`endpoint` or `auth` fails at load. Load failures are exit `2`: dragoman could not run.

**A missing environment variable is not a load failure.** It fails when the service is
actually called, naming the variable. An instance serving six providers must start with the
one key its operator has.

### Protocols own placement

> The **field** is structural, and dragoman maps it. The **value** is opaque and passes
> through byte for byte.

That rule already governs the model and the reasoning level. It governs the output cap and
the streaming decision too, and the quirks record was where it was being broken: a service
entry naming `max_completion_tokens` is a config file making a structural decision.

So `base.Quirks` is deleted, and each of its fields moves to the protocol that owns it:

| protocol | what it knows |
|---|---|
| `openai-responses` | the caller's own format; forwards the body when no flag overrides it |
| `openai-completions` | writes the cap to `max_tokens` |
| `openai-chat-completions` | writes the cap to `max_completion_tokens`, and asks for usage while streaming |
| `anthropic-messages` | requires a cap, supplies 20000 when nobody did, always streams upstream |
| `gemini-interactions` | writes the cap under `generation_config` |

**`openai-chat-completions` is the honest name for a split that already existed.** OpenAI's
chat endpoint and DeepSeek's disagree about the cap's field name and about whether usage
arrives unasked while streaming. Two wire shapes are two protocols; a dialect is a wire
shape, not a vendor, and calling both `openai-completions` was what forced a config key to
paper over the difference.

The two must not become two packages. `openaicompletions` keeps one encoder and one decoder
and exports two `Dialect` values built from a small variant record — the library provides the
engine, the variant provides the configuration. Copying an encoder and a decoder to change a
field name would be the actual N×M mistake this design exists to prevent.

**`anthropic-messages` always streams upstream, and that is not configurable.** Anthropic's
own SDKs require streaming at large output caps to avoid HTTP timeouts, and the cap this
protocol supplies when nobody did is such a value. There is no endpoint speaking this wire
format for which the answer differs, so it is a property of the format. It stays invisible
to the caller because folding a translated event stream is defined to produce exactly the
buffered response.

### A schema against a `no_schema` endpoint is dropped, never fatal

`text.format` is dropped, the drop is recorded, and the request proceeds. The caller gets a
`200` and prose. **A schema must never turn into a failed request.**

The reason is what the caller does next. Prose where JSON was expected reads as a model
quirk, and the caller does the thing they would have done without dragoman in the path: name
the shape in their own prompt and move on. A hard failure instead teaches them dragoman is
broken, over a field the backend never rejected — dragoman inventing a refusal on the
provider's behalf.

This is unchanged from today's behaviour. It is written down because it is the one place
where dropping a field is the *designed* outcome rather than a reported shortfall, and a
later reading of "silence is not an option" must not turn it into an error.

`--strict` still promotes it, as it promotes every drop. That is the flag's whole job, and a
caller who wants schema-or-nothing has it.

### Behaviour change

One, and it is cosmetic. **`openai-chat` changes protocol name**, from `openai-completions`
to `openai-chat-completions`. Same endpoint, same key, same bytes on the wire.

### The listener

```
POST /<service>/responses
```

The path segment is a key in the service table. Unknown segment is `404` naming the service
and listing what is served.

The body is an `openai-responses` request, unmodified. **Nothing about a request is
configured out of band**, because everything already travels in the body: `model`,
`reasoning.effort`, `max_output_tokens`, `stream`, `tools`, `tool_choice`, `text.format`,
`instructions`. The listener passes options carrying the service name and nothing else, and
the request drives the rest. This is why the surface is one path segment and not a flag set
per route.

Two supporting routes:

| route | purpose |
|---|---|
| `GET /services` | the table as JSON — name, protocol, endpoint, the variable each needs, and whether that variable is set. Never a key. |
| `GET /health` | liveness only |

`GET /services` is what makes a running instance self-describing, so a client discovers what
it can address without the operator's config file. `available` extends that to what would
answer right now: the flag says whether the named variable holds anything, never what.

**No client authentication.** The listener accepts any incoming request and binds
`127.0.0.1` by default. A shared bearer token would be authentication theatre in front of a
process holding provider keys; an instance that needs to be reachable belongs behind
something that does authentication as its actual job. The default bind is the honest
statement of that.

### Streaming is unchanged, and reframed

`stream` remains an **output** setting decided by the body, exactly as on the pipe. The
listener changes the framing and nothing else:

| the caller asked for | response |
|---|---|
| buffered | `Content-Type: application/json`, one Responses body |
| `stream: true` | `Content-Type: text/event-stream`, the same event sequence as SSE |

The event sequence is byte-identical to what the pipe writes as NDJSON — the whole Responses
lifecycle, not the deltas alone. Only `data: ` and a blank line are added, which is the point
the pipe transport already makes: framing belongs to the transport and never costs fidelity.
The bar is an unmodified SDK stream parser consuming it.

**Events are flushed per event.** Same correctness requirement as the pipe, same reason:
buffering them yields identical bytes and defeats the mode. The listener sets no write
deadline that could truncate a long answer.

Whether dragoman streams *from* a backend stays independent of what the caller receives.
Anthropic is streamed upstream to serve a buffered request, and the caller cannot tell.

### Errors are status codes

The pipe's three exit codes have a direct reading over HTTP, and the mapping has to preserve
what makes them trustworthy — that one class means "the backend said no" and the other means
"dragoman could not run":

| pipe | HTTP | when |
|---|---|---|
| `2` | `400` | unparseable body, a drop under strict, a required field with no source |
| `2` | `404` | unknown service |
| `2` | `500` | the service's `auth` variable is unset |
| `1` | the backend's own status | the backend refused |
| `0` | `200` | translated |

A missing credential is `500` and not `400`: the caller did nothing wrong, the instance is
misconfigured, and a `4xx` there would send an SDK into a retry loop over an operator error.

An error body is an `openai-responses` error object, because a client that only knows this
format has to be able to read the failure too.

**A backend refusal is returned with the backend's status.** Rate limits and quota errors
carry retry semantics the SDK already implements, and flattening them to `502` would discard
the one thing the client knows how to act on.

Once the first event of a stream is written the status line is already sent, so a mid-stream
failure ends the stream and is reported as an event. That is what the format has for it.

Drops go to the server log, not to the response, and remain the caller's signal on the pipe.
`--strict` on the listener applies to every request it serves.

### Out of scope

- **Client authentication, rate limiting, quotas, usage accounting.** Not this feature and
  arguably not this product.
- **Any per-service default for `model` or `effort`.** The body carries both. A server-side
  default would make the same request mean different things at different instances, which is
  the failure dragoman exists to remove.
- **Fan-out, fallback, retry across services.** A conversation lives on one backend for its
  whole life, because histories are not interchangeable between providers. A router that
  silently moved a conversation would corrupt it.
- **Config reload while running.** Restart. The process holds no state worth preserving.
- **`GET /responses/<id>` and any other stateful Responses route.** There is no store.

## Technical requirements

### The transport owns only framing

`internal/transport/serve/` is a sibling of `internal/transport/pipe/`, and the seam between
them is the one the first spec bought:

> The core takes a request and returns a response or an event sequence, and never touches
> `os.Stdout`.

So the listener is an addition and not a refactor. It reads the body, calls `core.Run` with
a `Sink` that writes SSE, and maps the returned `Failure` to a status. Flag precedence,
backend selection, translation and the dropped-field diff are core concerns it hands over and
never implements — the same rule the pipe follows. The exit-code-to-status mapping is the
listener's alone, because the pipe has no use for it.

### The table is parsed at a boundary into the existing type

`internal/config/` parses YAML into `map[string]base.Backend`, which is the type the core
already resolves against — no second backend type, and no `map[string]any` reaching the
core. `base.Backend` loses its `Quirks` field and becomes the three keys plus a name.

The default table is `//go:embed` of the same file format, so the shipped table is parsed by
the code under test rather than by a Go literal that could disagree with it. `catalog`
becomes the loader for that embedded file, keeping its `core.Catalog` constructor and losing
its hand-written preset map.

Dependencies flow unchanged — `config` imports `base`, the transports import `core`, `cmd/`
wires them. One dependency is added: `gopkg.in/yaml.v3`.

### Tests

The listener is tested over a real `net/http/httptest` server against the recorded fixtures
already in `testdata/fixtures/`, with the same `Executor` seam the pipe tests use. No mocks
and no network. Table-driven, and the cases that matter are the ones the design claims:

- the same request to `/<service>/responses` and through the pipe produces the same body,
  for every fixture and every backend — the listener adds framing and nothing else
- SSE reframing of a stream is the pipe's NDJSON sequence with `data: ` prefixes, event for
  event
- the status mapping above, each row
- a config replaces the table rather than merging: a file naming one service serves one
- a service whose `auth` variable is unset starts, is listed by `/services` as
  `available: false`, and fails `500` only when called
- an unknown `protocol` fails at load
- the embedded default table produces, service for service, the same backends the current
  preset map produces — modulo the two named behaviour changes. The migration is otherwise
  behaviour-preserving or it is a bug

**The fold invariant is what proves the quirks move was safe.** Moving the Anthropic
streaming decision out of a record and into the protocol changes which code path runs; the
invariant that folding a translated event stream equals the translated buffered response is
what makes that undetectable from outside, and it already runs for every fixture and every
backend.

`openai-chat-completions` is the least-covered path in this change: `testdata/fixtures/` has
four recorded backends and this is not one of them. It is tested through `--no-execute`
translation assertions — that the cap lands at `max_completion_tokens` and that a streaming
request carries `stream_options.include_usage` — and that is the honest extent of its
coverage until a fixture is recorded.
