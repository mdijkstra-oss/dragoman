# dragoman

> **dragoman** *(n.)* — an interpreter and guide, acting as both linguist and cultural mediator for travelers, merchants and diplomats.

Translates between LLM protocols — Responses in, translated to the target provider, Responses out.

## Services

A service is a name, an endpoint, a protocol, and the environment variable holding that endpoint's key. Six are built in:

| service | protocol | endpoint | key |
|---|---|---|---|
| `openai` | `openai-responses` | `https://api.openai.com/v1` | `OPENAI_API_KEY` |
| `anthropic` | `anthropic-messages` | `https://api.anthropic.com/v1` | `ANTHROPIC_API_KEY` |
| `gemini` | `gemini-interactions` | `https://generativelanguage.googleapis.com/v1beta` | `GEMINI_API_KEY` |
| `deepseek` | `openai-completions` | `https://api.deepseek.com` | `DEEPSEEK_API_KEY` |
| `openrouter` | `openai-completions` | `https://openrouter.ai/api/v1` | `OPENROUTER_API_KEY` |
| `openai-chat` | `openai-completions` | `https://api.openai.com/v1` | `OPENAI_API_KEY` |

A service whose variable is unset is still served. It reports itself unavailable on `GET /services` and fails when it is called, so one missing key does not stop the rest. Your own config can override the default set.

### Adding your own

`--config` names a YAML document. Every command that reads the table takes it.

```yaml
mode: append          # the default: these join the built-in services
groq:
  endpoint: https://api.groq.com/openai/v1
  protocol: openai-completions
  auth: GROQ_API_KEY
```

| field | what it changes |
|---|---|
| `endpoint` | scheme, host and base path. The protocol appends its own path to it. |
| `protocol` | which of the four wire formats this endpoint speaks. Required. |
| `auth` | the name of the variable holding the key, never the key. Required. |
| `max_output_tokens_as` | which field carries the output cap: `max_completion_tokens` or `max_tokens`. `openai-completions` only. |
| `schema_as_prompt` | see below. `openai-completions` only. |
| `omit_default_max_output_tokens` | omit Dragoman's synthesized 20,000-token cap while preserving caller-supplied caps. `openai-responses` only, for wrappers without cap control. |

`mode: override` disables the built in services. Under `append`, the built in services remain available.

> [!IMPORTANT]
> **`schema_as_prompt`.** Of the built-in services it applies to `deepseek` alone. [Its JSON mode](https://api-docs.deepseek.com/guides/json_mode) takes no schema and wants the shape described in the prompt, so an incoming `json_schema` is written into the instructions and sent with a `response_format` of `{"type": "json_object"}`. `text.format` is dropped on every such request.

## Supported Protocols

| protocol | path appended to the endpoint |
|---|---|
| `openai-responses` | `/responses` |
| `openai-completions` | `/chat/completions` |
| `anthropic-messages` | `/messages` |
| `gemini-interactions` | `/interactions` |

## Lossy translation

Not all protocols carry the same field. A field that cannot be carried across is dropped and named by its path in the logs on `--verbose`.
`--verbose` writes to stderr, `serve` logs them at debug level, and `--strict` turns any drop into an error before the call goes out.

## Quick start

Needs Go `1.25.1` and one provider key.

```console
$ go install github.com/mdijkstra-oss/dragoman/cmd/dragoman@latest
```

`config` prints what this binary would serve, in the same YAML a document uses.

```console
$ dragoman config
mode: override
anthropic:
  endpoint: https://api.anthropic.com/v1
  protocol: anthropic-messages
  auth: ANTHROPIC_API_KEY
...
```

The request arrives on stdin and the response leaves on stdout.

```console
$ export ANTHROPIC_API_KEY=...
$ echo '{"input":"Say hello in three words."}' \
    | dragoman --backend anthropic --model claude-sonnet-5 --max-tokens 100 --timing
elapsed 1.837s
{"id":"msg_011Cduw...","object":"response","status":"completed","model":"claude-sonnet-5","output":[{"type":"message","id":"msg_0","status":"completed","role":"assistant","content":[{"type":"output_text","text":"Hello, hi there!"}]}],"usage":{"input_tokens":14,"output_tokens":10,"total_tokens":24}}
```

`--stream` writes the event sequence instead, one JSON object per line. `--no-execute` prints the translated request and makes no call. `--raw` returns the backend's answer untranslated.

The same translation is served over HTTP, where `stream` comes back as server-sent events:

```console
$ dragoman serve --addr 127.0.0.1:8080

$ curl -sN -X POST http://127.0.0.1:8080/responses \
    -d '{"model":"gemini/gemini-3.6-flash","input":"Say hello.","stream":true}'
event: response.created
data: {"type":"response.created","sequence_number":1,"response":{"id":"v1_Chds...","status":"in_progress","model":"gemini-3.6-flash","output":[]}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","sequence_number":7,"item_id":"msg_1","output_index":1,"content_index":0,"delta":"Hello!"}
```

## HTTP API

| route | body | response |
|---|---|---|
| `POST /responses` | an `openai-responses` request | an `openai-responses` response, or server-sent events when the body sets `stream` |
| `GET /services` | — | listing of available services |
| `GET /health` | — | `{"status":"ok"}` when running |

Over HTTP there is no `--backend`: the model names its service. The split is on the first slash only, so `openrouter/anthropic/claude-3` reaches `openrouter` and asks it for `anthropic/claude-3`.

A provider's own status is passed through, so a rate limit keeps the retry semantics an SDK already implements; a provider error arriving inside a `200` becomes `502`.

## Runtime environment

Every service names the variable holding its key, and those names are the table's. The listen address is a flag, not a variable.

`LOG_REQUEST_HEADERS` names caller headers to put on every log line for the request — `X-` names only, at most 16, and a name reading as a credential is refused at boot.

`TRANSLATE_ONLY` set to `1`, `true` or `yes` is `--no-execute` over HTTP: every answer is the body the backend would have received, and no call goes out. Nothing is spent and no key is needed, so an instance in this mode runs on a machine holding none — which is what it is for, and why it is off by default. The startup line says which it is.

The mode is the instance, not the request. A listener either translates or serves, because one that did both would put a real call one forgotten parameter away.

```console
$ TRANSLATE_ONLY=1 dragoman serve --addr 127.0.0.1:8080

$ curl -s -X POST http://127.0.0.1:8080/responses \
    -d '{"model":"anthropic/claude-sonnet-5","input":"Say hello."}'
{"model":"claude-sonnet-5","max_tokens":20000,"messages":[{"role":"user","content":[{"type":"text","text":"Say hello."}]}]}
```

Logs are JSON on stderr:

```json
{"time":"2026-08-11T01:33:23.323898+02:00","level":"INFO","msg":"request served","service":"anthropic","model":"claude-sonnet-5","status":200,"duration_ms":2044,"input_tokens":11,"cached_tokens":0,"output_tokens":17,"reasoning_tokens":0,"total_tokens":28,"headers":{"x-session-id":"s-42"}}
```

## Deployment

```sh
docker build -t dragoman .
docker run -p 8080:8080 -e ANTHROPIC_API_KEY dragoman
```

`Dockerfile` builds a static binary onto `scratch`, listening on `0.0.0.0:8080`. It carries no key and no configuration document, so the image is the same bytes for every deployment. Nothing outside this repository is read, so a git URL is a complete build context.

## Development

```sh
make run PROMPT="Say hello."   # one request against the default model
make run < request.json        # a whole request off stdin
make serve                     # the built-in table over HTTP
make serve-dev                 # the same, rebuilt and restarted on a change
make bridge                    # anthropic-messages answered by the claude CLI
make codex-bridge              # Responses answered by the codex CLI
go test ./...
```

`MODEL`, `REASONING` and `ADDR` override the defaults, `ARGS` goes straight to dragoman, and `JQ=.` prints the whole response instead of a summary. Keys come from `.env.local`, which `.env.example` describes.

The service table is embedded, so editing it is a source change and rebuilds the binary.

`make bridge` starts [claude-bridge](cmd/claude-bridge/README.md), a development-only server that answers `anthropic-messages` requests through a signed-in Claude Code CLI instead of an API key. No tools, one turn per request; its README lists the limits.

`make codex-bridge` starts [codex-bridge](cmd/codex-bridge/README.md), a development-only Responses server that runs one isolated, tool-disabled Codex CLI turn against the signed-in ChatGPT account. It buffers each CLI result; `stream: true` receives valid but delayed SSE with one text delta.

## CLI

Every command takes `--config`, except `healthcheck`.

| command | does |
|---|---|
| *(default)* | Translate one request from stdin to stdout. |
| `serve` | Serve the service table over HTTP. |
| `config` | Print the table this binary would serve. |
| `healthcheck` | Report whether a listener is serving. |

No flag takes a credential — only the name of a variable — because argv is visible in `ps`, in shell history and in CI logs.

Exit codes are `0` for an answer, `1` for a backend that refused, and `2` for a request that was never sendable.

## License

AGPL-3.0. The full text is in [LICENSE](./LICENSE).

## See also

- [chancery](https://github.com/mdijkstra-oss/chancery) — an agent server built from Markdown files. Each file is a prompt and its own HTTP route.
