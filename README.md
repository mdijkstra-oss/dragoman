# dragoman

A stateless protocol-translating LLM proxy.

dragoman accepts a request in the `openai-responses` format, sends it to the target provider in that provider's own format, and converts the reply back to `openai-responses`. A client needs one request format and one response parser (e.g. a stock OpenAI library).

```sh
# One request over a pipe, against a service the binary ships with
dragoman --backend anthropic --model claude-sonnet-5 < request.json
dragoman --backend openrouter --model anthropic/claude-sonnet-5 --stream < request.json

# An endpoint of your own
dragoman --backend-protocol openai-completions --endpoint https://llm.internal.example/v1 \
         --auth-env INTERNAL_LLM_KEY --model my-local-model < request.json

# A listener, for a client that speaks to a base URL
dragoman serve --addr 127.0.0.1:8080
```

`--file` reads the request from a path instead of stdin, `--output` writes the result to a path instead of stdout, and `-` means the stream either way.

## Services

A service is a name for a provider. dragoman ships with six, embedded in the binary:

| service | set this variable | endpoint | protocol |
|---|---|---|---|
| `openai` | `OPENAI_API_KEY` | `https://api.openai.com/v1` | `openai-responses` |
| `openai-chat` | `OPENAI_API_KEY` | `https://api.openai.com/v1` | `openai-completions` |
| `anthropic` | `ANTHROPIC_API_KEY` | `https://api.anthropic.com/v1` | `anthropic-messages` |
| `gemini` | `GEMINI_API_KEY` | `https://generativelanguage.googleapis.com/v1beta` | `gemini-interactions` |
| `deepseek` | `DEEPSEEK_API_KEY` | `https://api.deepseek.com` | `openai-completions` |
| `openrouter` | `OPENROUTER_API_KEY` | `https://openrouter.ai/api/v1` | `openai-completions` |

Export the variable, name the service with `--backend` on the pipe or as the first segment of `model` on the listener, and there is nothing else to set up. The variable is read when a request names the service, never at startup, so one instance serves every row whatever the environment holds.

Anything else is [an endpoint of your own](#bring-your-own-endpoint), reached by flags or by a config file. Both need the same three things: the URL, the protocol, and the name of the variable holding the key.

## Protocols

Four wire formats, each reached at its own path under the endpoint and each keeping the reasoning level and the output cap somewhere different:

| protocol | request path | reasoning level | output cap |
|---|---|---|---|
| `openai-responses` | `/responses` | `reasoning.effort` | `max_output_tokens` |
| `openai-completions` | `/chat/completions` | `reasoning_effort` | `max_completion_tokens`, or `max_tokens` where `max_output_tokens_as` says so |
| `anthropic-messages` | `/messages` | `output_config.effort` | `max_tokens` |
| `gemini-interactions` | `/interactions` | `generation_config.thinking_level` | `generation_config.max_output_tokens` |

An endpoint of `https://api.anthropic.com/v1` on `anthropic-messages` is called at `https://api.anthropic.com/v1/messages`.

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
| `--endpoint` | `endpoint` | base URL; the [request path](#protocols) comes from the protocol | required |
| `--backend-protocol` | `protocol` | `openai-responses`, `openai-completions`, `anthropic-messages` or `gemini-interactions` | required; one of those four |
| `--auth-env` | `auth` | **name of the environment variable** holding the key, not the key | required |
| — | `max_output_tokens_as` | which field carries the output cap, `max_tokens` or `max_completion_tokens` | `openai-completions` only |
| — | `schema_as_prompt` | `true` where the endpoint has a JSON mode and no schema mode | `openai-completions` only |

A config file **replaces** the embedded table wholesale; there is no merge, so a config naming one service serves that one service. Run `dragoman config` to print the embedded table and edit a copy of it.

Neither optional key has a flag. An endpoint that needs one needs a table entry.

`schema_as_prompt` applies to `deepseek` alone. [Its JSON mode](https://api-docs.deepseek.com/guides/json_mode) takes no schema and wants the shape described in the prompt, so an incoming `json_schema` is written into `instructions` ahead of the caller's own prompt and sent with a `response_format` of `{"type": "json_object"}`. `text.format` is [dropped](#dropped-fields) and the drop is recorded on every such request.

> [!IMPORTANT]
> A schema in a prompt is a request, not a constraint. An answer that is valid JSON of the wrong shape is a parse failure with nothing in the exchange to explain it, so the drop is worth reading rather than skimming past.

## Model, effort and output cap

Precedence is the `--model`/`--effort`/`--max-tokens` flag, then the value in the body.

The reasoning level passes through byte for byte: never normalised, case-folded, validated against a set, or mapped onto a nearer neighbour. Reasoning levels do not denote comparable strength across providers. A level nobody asked for stays absent, because filling one in overrides a provider default and changes both cost and behaviour.

The output cap is the one value dragoman supplies unasked. Where neither flag nor body carries `max_output_tokens`, every backend receives 20000. `anthropic-messages` will not accept a request without a cap at all, and on a reasoning model the cap covers thinking and output together, so a small one truncates mid-thought.

## Streaming

`stream` is an **output** setting. It determines what the caller receives, not how dragoman calls the backend.

- default — one `openai-responses` body, written once, when the answer is complete.
- `stream: true` in the body, or `--stream` on the pipe — the `openai-responses` event sequence, written and flushed per event.

What is emitted is the complete Responses lifecycle, not the deltas alone, so a stock SDK reconstructs the answer from that sequence the way it always has. The pipe writes the sequence as newline-delimited JSON, and the listener writes the identical sequence as SSE.

`anthropic-messages` is always streamed from the backend, whatever the caller asked for, because its own SDKs require streaming at the cap that protocol needs. A caller that asked for a buffered answer cannot tell.

## Dropped fields

A drop is any key present in a body that no typed field consumed. An entry carries a path rather than a name (`input[3].content[1].signature`), because dragoman skips the key and continues, and the path is the caller's only signal.

| reason | meaning |
|---|---|
| `unsupported` | the target protocol has no equivalent and never will |
| `unimplemented` | dragoman could map it and does not yet |
| `inert` | the target has no equivalent and needs none: the field could not have changed the answer |

A schema's name is a label, and `strict: true` against a protocol that enforces a schema anyway is asking for what the request gets — both are inert.

By default the response is returned and the drops that changed the exchange go to stderr on the pipe and to the server log as a warning on the listener, never into the answer. A dropped field nobody was told about produces a confident, well-formed, wrong answer. Inert drops are kept for `-v` on the pipe and for the debug log on the listener, because a warning repeated on every request of a kind is how the warning that matters gets skimmed past.

`--strict` makes any drop an error, inert included: exit `2`, nothing sent. `dragoman serve --strict` applies the same rule to every request that listener serves.

## Provider state

Every provider returns something it wants handed back unaltered, and each binds it to the item it arrived on. dragoman carries it in the item's `encrypted_content` and puts it back where it belongs — a signature on an Anthropic thinking block, a Gemini thought signature (which may ride on the call itself), DeepSeek's reasoning content, OpenAI's encrypted reasoning.

A conversation lives on one backend for its whole life. Histories are not interchangeable between providers and never could be.

Because dragoman is stateless, that state survives a turn boundary only in the client's hands: echo the response's output items back in the next request's input. An application that reconstructs history from rendered text will drop it.

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

## Serving

The pipe answers one request per process, which most clients cannot use: an SDK points at a base URL, and that is the whole of its integration surface.

```sh
dragoman serve --addr 127.0.0.1:8080 --config ./dragoman.yaml
```

| route | purpose |
|---|---|
| `POST /responses` | an `openai-responses` request whose `model` names its service first: `openai/gpt-5.5` |
| `GET /services` | the table as JSON — name, protocol, endpoint, the variable each needs, and whether that variable is set. Never a key |
| `GET /health` | liveness only, answering `{"status":"ok"}` |

**The service is the first segment of `model`, split on the first `/` only.** `openai/gpt-5.5` reaches `openai` with `gpt-5.5` on the forwarded body, and `openrouter/anthropic/claude-3` reaches `openrouter` with `anthropic/claude-3`, because a provider whose own identifiers are paths has to stay addressable. A model with no service, or one naming a service this instance does not serve, is a `404` listing what it does. Prefixing the model is how OpenRouter spells the same idea, so a client already writing one needs no field it does not have.

Every field that shapes the answer travels in the body — `model`, `reasoning.effort`, `max_output_tokens`, `stream`, `tools`, `tool_choice`, `text.format` and `instructions` — so one base URL is a client's whole integration surface. `--config` and `--strict` are the only things a listener settles, and both apply to every request it serves. An application changes provider by changing a model name.

One query parameter changes what arrives: `POST /responses?prompt_cache_breakpoints=false` removes every `prompt_cache_breakpoint` from the body, along with the `prompt_cache_options` that carries their TTL, before any translation happens. Whether explicit breakpoints are accepted is a property of the model rather than of the protocol, and a caller in front of dragoman is often too far upstream to know which model answers. `prompt_cache_key` stays, because implicit caching is a separate mechanism. Any other value, and a request that omits the parameter, travels as it arrived.

`stream` is decided by the body exactly as on the pipe, and only the framing changes: a buffered request gets `application/json` and one Responses body, a streaming one gets `text/event-stream` carrying the same event sequence, flushed per event.

> [!WARNING]
> There is **no client authentication**, and the default bind is `127.0.0.1`. An instance that needs to be reachable belongs behind something that does authentication as its actual job.

The pipe's [exit codes](#exit-codes) and the listener's statuses line up:

| code | HTTP | when |
|---|---|---|
| `2` | `400` | unparseable body, a required field with no source, a drop under `--strict` |
| `2` | `404` | a `model` naming no service this instance serves, or a path that is not a route |
| `2` | `500` | the service's `auth` variable is unset |
| `1` | the backend's own status | the backend refused |
| `0` | `200` | translated |

Because a key is read per request rather than at startup, one absent key never takes down the services beside it, and `GET /services` reports `available` per row. Two of its rows:

```json
[
  {"name":"openai","protocol":"openai-responses","endpoint":"https://api.openai.com/v1","auth":"OPENAI_API_KEY","available":true},
  {"name":"gemini","protocol":"gemini-interactions","endpoint":"https://generativelanguage.googleapis.com/v1beta","auth":"GEMINI_API_KEY","available":false}
]
```

A client can pick a model it knows will answer rather than discovering the gap on the call. The flag says whether the named variable holds anything, not what.

A missing credential is `500` and not `400`: the caller did nothing wrong, the instance is misconfigured, and a `4xx` there would send an SDK into a retry loop over an operator error. A refusal keeps the backend's own status, because a rate limit carries retry semantics the SDK already implements; a provider error that arrived on a `200` becomes `502`. Failures are `openai-responses` error objects — a client that knows only this format has to be able to read them too. Once the first event of a stream is written the status line is already sent, so a mid-stream failure ends the stream as an event.

### Request log

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

## Running in a container

The image is a static binary on `scratch`, running as `65532:65532`, and it serves on `0.0.0.0:8080` unless the command is overridden:

```sh
docker build -t dragoman .
docker run -p 8080:8080 -e ANTHROPIC_API_KEY dragoman
```

No key and no service table are baked in, so the image is the same bytes for every deployment: keys arrive as environment variables, and a table other than the embedded one is mounted read-only and named with `--config`. `dragoman healthcheck --addr 127.0.0.1:8080` asks whether this process is serving and is what the image's own `HEALTHCHECK` runs; it reaches no provider, so an instance is not restarted for someone else's outage.

## Building and testing

```sh
go build -o bin/dragoman ./cmd/dragoman
go test ./...
```

The suite runs offline against responses recorded from live backends in `testdata/fixtures/`, and over a real `httptest` server for the listener. No key and no network are needed. Each recorded request was produced by `--no-execute --stream` and then POSTed to the live endpoint, so an encoder change fails the suite until the fixture is recaptured, which re-proves acceptance rather than assuming it.

OpenAI's own chat endpoint has no recorded fixture, so `openai-chat` is covered by translation assertions rather than by a round trip.

## License

AGPL-3.0. The full text is in [LICENSE](./LICENSE).

AGPL-3.0 is strong copyleft, and its network clause applies where no copy is distributed at all: anyone who runs a modified dragoman as a network service must offer the source of that version to the users that service reaches. Read the terms before deploying a modified copy, not after.
