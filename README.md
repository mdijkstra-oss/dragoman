# dragoman

A stateless protocol-translating LLM proxy.

dragoman accepts a request in the `openai-responses` format, sends it to the target provider in that provider's own format, and converts the reply back to `openai-responses`. A client needs one request format and one response parser (e.g. standard OpenAI library).

```sh
# Direct on CLI - baked in endpoints
dragoman --backend anthropic --model claude-sonnet-5 < request.json
dragoman --backend openrouter --model anthropic/claude-sonnet-5 --stream < request.json

# Custom endpoint and protocol
dragoman --backend-protocol openai-completions --endpoint https://llm.internal.example/v1 \
         --auth-env INTERNAL_LLM_KEY --model my-local-model < request.json

# Start a webservice - use config.yaml to define available backends
dragoman serve --addr 127.0.0.1:8080
```

## Services

A service is a name for a provider. dragoman ships with seven, embedded in the binary:

| service | set this variable | endpoint | protocol |
|---|---|---|---|
| `openai` | `OPENAI_API_KEY` | `https://api.openai.com/v1` | `openai-responses` |
| `openai-chat` | `OPENAI_API_KEY` | `https://api.openai.com/v1` | `openai-completions` |
| `anthropic` | `ANTHROPIC_API_KEY` | `https://api.anthropic.com/v1` | `anthropic-messages` |
| `gemini` | `GEMINI_API_KEY` | `https://generativelanguage.googleapis.com/v1beta` | `gemini-interactions` |
| `deepseek` | `DEEPSEEK_API_KEY` | `https://api.deepseek.com` | `openai-completions` |
| `openrouter` | `OPENROUTER_API_KEY` | `https://openrouter.ai/api/v1` | `openai-completions` |
| [`claude-cli`](#the-local-cli) | none | `exec:claude` | `anthropic-messages` |

Export the variable, name the service with `--backend` on the pipe or as the first segment of `model` on the listener, and there is nothing else to set up. The variable is read when a request names the service, never at startup, so an instance serves every row whatever the environment holds.

Anything else is [an endpoint of your own](#bring-your-own-endpoint), reached by flags or by a config file. Both need the same three things: the URL, the protocol, and the name of the variable holding the key.

## Bring your own endpoint

An endpoint dragoman does not ship with is the same settings either way — as flags, or as an entry in a YAML file `--config` points at:

```sh
dragoman --backend-protocol anthropic-messages \
         --endpoint https://llm.internal.example/v1 \
         --auth-env INTERNAL_LLM_KEY \
         --model my-local-model < req.json
```

```yaml
internal:
  endpoint: https://llm.internal.example/v1
  protocol: anthropic-messages
  auth: INTERNAL_LLM_KEY
```

| flag | config key | what to pass | validation |
|---|---|---|---|
| `--endpoint` | `endpoint` | base URL; the operation path comes from the protocol, so `anthropic-messages` is reached at `/messages` | required |
| `--backend-protocol` | `protocol` | `openai-responses`, `openai-completions`, `anthropic-messages` or `gemini-interactions` | required; one of those four |
| `--auth-env` | `auth` | **name of the environment variable** holding the key, not the key | required, unless the endpoint is a local process |
| — | `max_output_tokens_as` | which field carries the output cap, `max_tokens` or `max_completion_tokens` | `openai-completions` only |
| — | `schema_as_prompt` | `true` where the endpoint has a JSON mode and no schema mode | `openai-completions` only |

A config file **replaces** the embedded table wholesale; there is no merge, so a config naming one service serves that one service. Run `dragoman config` to print the embedded table and edit a copy of it.

`schema_as_prompt` applies to `deepseek` alone. [Its JSON mode](https://api-docs.deepseek.com/guides/json_mode) takes no schema and wants the shape described in the prompt, so an incoming `json_schema` is written into `instructions` ahead of the caller's own prompt and sent with a `response_format` of `{"type": "json_object"}`.

> [!IMPORTANT]
> A schema in a prompt is a request, not a constraint. `strict` is dropped along with the field that carried it, and the drop is recorded on every such request.

Neither optional key has a flag. An endpoint that needs one needs a table entry.

## The local CLI

One service is a process rather than a URL. `claude-cli` spawns a locally installed Claude Code, which authenticates itself, so a developer with a subscription and no API key can route a request through it:

```sh
dragoman --backend claude-cli --model sonnet < request.json
```

It is a row of the same table as every other service, so the listener serves it too — a request naming `claude-cli/sonnet` as its model reaches the same backend the pipe does, and `GET /services` reports it with an empty `auth`.

`--backend claude-cli` is sugar for `--backend-protocol anthropic-messages --endpoint exec:claude`, and the `exec:` scheme is what selects a process. The scheme also carries what such a backend cannot do, so naming the binary in flags behaves exactly as the table entry does.

The CLI is an agent by default — it reads `CLAUDE.md`, runs hooks, resolves skills and plugins, persists a session and executes tools. None of that was asked for by a caller who sent a Responses body, so dragoman fixes the flags that turn it back into a model endpoint and does not make them configurable.

Reaching an agent harness costs two capabilities, and both are dropped loudly rather than ignored:

| dropped | why |
|---|---|
| `tools[n]`, `tool_choice`, `parallel_tool_calls` | the CLI resolves its own tools and has no channel to hand a call back |
| `max_output_tokens` | it has no flag for an output cap |

A second **user** message is a refusal rather than a drop: exit `2`, naming every user turn in the input. The CLI runs a turn per user message, so sending one would run an extra turn whose answer nobody asked for, and dropping one would change the conversation. A tool result counts as a user turn. A one-shot client works against this service; a chat client does not.

Two preflight checks run before the call, both exit `2`: the binary is on `PATH`, and `claude auth status` reports a signed-in session. Neither costs a token.

## Model, effort and output cap

Each protocol keeps the reasoning level somewhere different, and dragoman moves it there:

| protocol | reasoning level lives at |
|---|---|
| `openai-responses` | `reasoning.effort` |
| `openai-completions` | `reasoning_effort` |
| `anthropic-messages` | `output_config.effort` |
| `gemini-interactions` | `generation_config.thinking_level` |

The value itself passes through byte for byte: never normalised, case-folded, validated against a set, or mapped onto a nearer neighbour. Reasoning levels do not denote comparable strength across providers.

Precedence is the `--model`/`--effort`/`--max-tokens` flag, then the value in the body, then nothing. Absent stays absent — filling in a level nobody asked for silently overrides a provider default and changes both cost and behaviour.

`anthropic-messages` is the only protocol that rejects a request without an output cap. When neither flag nor body supplies one, that protocol supplies 20000; no other backend ever receives an invented value. The protocol also decides where the cap lands — `max_tokens`, `max_completion_tokens`, or under `generation_config` — unless a service entry sets `max_output_tokens_as`.

## Streaming

`stream` is an **output** setting. It determines what the caller receives, not how dragoman calls the backend. On the pipe:

- default — one `openai-responses` body on stdout, written once, when the answer is complete.
- `stream: true` in the body, or `--stream` — the `openai-responses` event sequence, one event per line, written and flushed per event.

What is emitted is the complete Responses lifecycle, not the deltas alone, so a stock SDK reconstructs the answer from that sequence the way it always has. The pipe writes the sequence as newline-delimited JSON, and the listener writes the identical sequence as SSE.

`anthropic-messages` is always streamed from the backend, whatever the caller asked for, because its own SDKs require streaming at the cap that protocol supplies. A caller that asked for a buffered answer cannot tell.

The two axes are independent, and one test holds them together: folding a translated event stream equals the translated buffered response, for every fixture and every backend.

## Dropped fields

A drop is any key present in a body that no typed field consumed. An entry carries a path rather than a name (`input[3].content[1].signature`), because dragoman skips the key and continues, and the path is the caller's only signal.

- **unsupported** — the target protocol has no equivalent and never will.
- **unimplemented** — dragoman could map it and does not yet.
- **inert** — the target has no equivalent and needs none: the field could not have changed the answer. A schema's name is a label; `strict: true` against a protocol that enforces a schema anyway is asking for what the request gets.

By default the response is returned and the drops that changed the exchange go to stderr on the pipe and to the server log as a warning on the listener, never into the answer. A dropped field nobody was told about produces a confident, well-formed, wrong answer. An inert drop is kept for `-v` on the pipe and for the debug log on the listener, because a warning repeated on every request of a kind is how the warning that matters gets skimmed past.

`--strict` makes any drop an error, inert included: exit `2`, nothing sent.

A schema sent to a service marked `schema_as_prompt` is the one drop that is the designed outcome: `text.format` goes, the drop is recorded, and the shape reaches the model as prose instead. What the caller loses is enforcement, which is worth saying on every request — an answer that is valid JSON of the wrong shape is a parse failure with nothing in the exchange to explain it. `--strict` promotes it like any other.

## Provider state

Every provider returns something it wants handed back unaltered, and each binds it to the item it arrived on. dragoman carries it in the item's `encrypted_content` and puts it back where it belongs — a signature on an Anthropic thinking block, a Gemini thought signature (which may ride on the call itself), DeepSeek's reasoning content, OpenAI's encrypted reasoning.

A conversation lives on one backend for its whole life. Histories are not interchangeable between providers and never could be.

Because dragoman is stateless, that state survives a turn boundary only in the client's hands: echo the response's output items back in the next request's input. An application that reconstructs history from rendered text will drop it.

## Serving

The pipe answers one request per process, which most clients cannot use: an SDK points at a base URL, and that is the whole of its integration surface.

```sh
dragoman serve --addr 127.0.0.1:8080 --config ./dragoman.yaml
```

| route | purpose |
|---|---|
| `POST /responses` | an `openai-responses` request whose `model` names its service first: `openai/gpt-5.5` |
| `GET /services` | the table as JSON — name, protocol, endpoint, the variable each needs, and whether that variable is set. Never a key |
| `GET /health` | liveness only |

**The service is the first segment of `model`, split on the first `/` only.** `openai/gpt-5.5` reaches `openai` with `gpt-5.5` on the forwarded body, and `openrouter/anthropic/claude-3` reaches `openrouter` with `anthropic/claude-3`, because a provider whose own identifiers are paths has to stay addressable. A model with no service, or one naming a service this instance does not serve, is a `404` listing what it does. Prefixing the model is how OpenRouter spells the same idea, so a client already writing one needs no field it does not have.

Every field that shapes the answer travels in the body — `model`, `reasoning.effort`, `max_output_tokens`, `stream`, `tools`, `tool_choice`, `text.format` and `instructions` — so one base URL is a client's whole integration surface. `--config` and `--strict` are the only things a listener settles, and both apply to every request it serves. An application changes provider by changing a model name.

One query parameter changes what arrives: `POST /responses?prompt_cache_breakpoints=false` removes every `prompt_cache_breakpoint` from the body, along with the `prompt_cache_options` that carries their TTL, before any translation happens. Whether explicit breakpoints are accepted is a property of the model rather than of the protocol, and a caller in front of dragoman is often too far upstream to know which model answers. `prompt_cache_key` stays, because implicit caching is a separate mechanism. Any other value, and a request that omits the parameter, travels as it arrived.

`stream` is decided by the body exactly as on the pipe, and only the framing changes: a buffered request gets `application/json` and one Responses body, a streaming one gets `text/event-stream` carrying the same event sequence, flushed per event.

**No client authentication**, and the default bind is `127.0.0.1`. An instance that needs to be reachable belongs behind something that does authentication as its actual job.

The pipe's [exit codes](#exit-codes) and the listener's statuses line up:

| code | HTTP | when |
|---|---|---|
| `2` | `400` | unparseable body, a required field with no source, a drop under `--strict` |
| `2` | `404` | a `model` naming no service this instance serves |
| `2` | `500` | the service's `auth` variable is unset |
| `1` | the backend's own status | the backend refused |
| `0` | `200` | translated |

Because a key is read per request rather than at startup, one absent key never takes down the services beside it, and `GET /services` reports `available` per row. Two of its rows:

```json
[
  {"name":"claude-cli","protocol":"anthropic-messages","endpoint":"exec:claude","auth":"","available":true},
  {"name":"gemini","protocol":"gemini-interactions","endpoint":"https://generativelanguage.googleapis.com/v1beta","auth":"GEMINI_API_KEY","available":false}
]
```

A client can pick a model it knows will answer rather than discovering the gap on the call. The flag says whether the named variable holds anything, not what, and a service naming no variable is always available because it carries its own credential.

A missing credential is `500` and not `400`: the caller did nothing wrong, the instance is misconfigured, and a `4xx` there would send an SDK into a retry loop over an operator error. A refusal keeps the backend's own status, because a rate limit carries retry semantics the SDK already implements; a provider error that arrived on a `200` becomes `502`. Failures are `openai-responses` error objects — a client that knows only this format has to be able to read them too. Once the first event of a stream is written the status line is already sent, so a mid-stream failure ends the stream as an event.

## Accounting

The listener logs JSON, and a request that worked is one line of it:

```json
{"time":"2026-07-31T12:04:11Z","level":"INFO","msg":"request served","service":"anthropic","model":"claude-4-6","status":200,"duration_ms":842,"input_tokens":516,"cached_tokens":0,"output_tokens":160,"reasoning_tokens":34,"total_tokens":676}
```

The counts are the normalised ones — the same key means the same thing whichever provider answered — written whether the caller streamed or not, and at zero rather than absent when a backend reported none, because a line whose key set depends on who answered cannot be summed. `duration_ms` is dragoman's exchange with the backend, never a caller's view of its own request. A failed request writes the failure line and not this one.

`LOG_REQUEST_HEADERS` adds a `headers` group to that line, which is how a caller in front of dragoman puts its own identity beside the cost — a request ID shared with its own log, a session, whichever agent it resolved:

```sh
LOG_REQUEST_HEADERS=X-Request-ID,X-Session-ID dragoman serve
```

**An allowlist, never a passthrough.** A request carrying fifty headers produces entries for the names configured here or nothing; unset records nothing at all. A name the request omits is left out rather than written empty. At most sixteen names, each canonicalizing to `X-*`, none containing a credential-like term (`auth`, `api-key`, `secret`, `password` among them) — a violation fails at load: exit `2`, naming the entry. An operator who can name any header to log can name the one carrying a key, and a key in a log is a key in every archive of it.

A recorded value is a label. dragoman performs no authentication, so a header naming a subject grants nothing, reaches no routing decision, and selects no credential.

## Diagnostics

| flag | effect |
|---|---|
| `--no-execute` | translate the request, print what would have been sent, make no call |
| `--raw` | return the backend's untranslated response |
| `-v` | the translation and every dropped field, inert ones included, to stderr |
| `--timing` | elapsed round trip, and time to the first event, to stderr |

`--raw` returns exactly what the backend sent. For a protocol that streams upstream regardless of the output mode — `anthropic-messages` — that is the event transcript rather than a body.

## Exit codes

| code | meaning |
|---|---|
| `0` | a response was returned and translated |
| `1` | the backend said no: HTTP 4xx/5xx, or a provider error body |
| `2` | dragoman could not run: unparseable input, unknown protocol, a service table that will not load, a log-header allowlist that will not, unknown service, missing key, bad flags, a required field with no source, or a drop under `--strict` |

`2` covers every case where the tool could not run, and nothing else, which is what makes it usable in a hook or a CI step.

## Layout

Dependencies flow one way. The base imports nothing; protocol packages import the base only, never each other; transports import the core; `cmd/` wires them together.

| path | holds |
|---|---|
| `internal/base/` | the `openai-responses` format — types, the event grammar, the emitter, the fold, the drop diff |
| `internal/dialect/` | the shape every wire format implements |
| `internal/dialect/*/` | one package per protocol |
| `internal/config/` | the service table — YAML in, backends out, and the embedded default |
| `internal/core/` | flag precedence, backend selection, the pipeline, and the executors that reach a backend over HTTP or by spawning a local CLI |
| `internal/catalog/` | the registered protocols, and the table built out of them |
| `internal/transport/pipe/` | stdin to stdout |
| `internal/transport/serve/` | HTTP in, one body or an SSE stream out |
| `cmd/dragoman/` | argv, streams, exit codes — and nothing else |

The core takes a request and returns a response or an event sequence, and never touches `os.Stdout`. The listener reads a body, hands the core a sink that writes SSE, and maps the failure it gets back to a status.

## Building and testing

```sh
go build -o bin/dragoman ./cmd/dragoman
go test ./...
```

The suite runs offline against responses recorded from live backends in `testdata/fixtures/`, and over a real `httptest` server for the listener. No key and no network are needed.

OpenAI's own chat endpoint has no recorded fixture, so `openai-chat` is covered by translation assertions rather than by a round trip.
