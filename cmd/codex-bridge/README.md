# codex-bridge

`codex-bridge` is a development-only HTTP server that speaks the OpenAI Responses protocol and answers each request by spawning one `codex exec --json` process. It uses the ChatGPT account signed into the local Codex CLI rather than an API key.

> [!WARNING]
> This is not the Responses API. ChatGPT plan eligibility, limits, and permitted use are governed by the applicable OpenAI terms. The bridge is intentionally narrow and is not a production server.

## Contract

The bridge accepts `POST /responses` and `POST /v1/responses`, buffered or with `stream: true`. A request may contain:

- a required `model`;
- optional `instructions` and `reasoning.effort`;
- exactly one user text input, plus any number of system/developer text messages;
- text output or a JSON Schema in `text.format`;
- `store` absent or `false`, plus empty or null inert fields such as `tools: []`.

History, non-user messages, images, caller tools and tool results, reasoning summaries, `previous_response_id`, output-token limits, persistence, and every other unsupported non-empty field receive `400`. CLI startup, execution, or JSONL protocol failures receive `502`.

Streaming is wire-compatible SSE. The bridge immediately emits `response.created` and `response.in_progress`, then writes a comment keepalive every 25 seconds while `codex exec` runs. It emits the completed answer in one `response.output_text.delta`. Buffered and streaming responses report the same completed text and usage.

Every request is independent. There are no persistent sessions, injected thread IDs, history, caller tools, MCP servers, or app-server protocol.

## Run

The bridge accepts the installed Codex CLI version. Authenticate through ChatGPT, then start it:

```console
$ codex login
$ codex login status
Logged in using ChatGPT

$ go run ./cmd/codex-bridge
$ make codex-bridge
```

| flag | default | does |
|---|---|---|
| `--addr` | `127.0.0.1:8083` | Address to listen on. |
| `--binary` | `codex` | Codex executable name or path. |
| `-v`, `--verbose` | off | Log each Codex JSONL frame type. |
| `--web` | off | Let Codex use its own web search. It runs inside the CLI and comes back as text; the caller still sees no tool calls. |

`GET /health` returns `{"ok":true,"account":"ChatGPT"}`. The server has no client authentication and is designed for loopback use only. Do not expose it on a network.

## Point Dragoman at it

The included [dragoman.yaml](dragoman.yaml) adds a local `codex` service. Its `omit_default_max_output_tokens` setting prevents Dragoman from adding its normal 20,000-token fallback because the CLI has no verified output-cap mapping; a cap explicitly supplied by the caller is preserved and refused by the bridge. Dragoman requires the named auth variable to exist, although neither Dragoman nor the bridge sends its dummy value to Codex:

```console
$ CODEX_BRIDGE_KEY=x dragoman serve --config cmd/codex-bridge/dragoman.yaml

$ curl -s localhost:8080/responses \
    -H 'content-type: application/json' \
    -d '{"model":"codex/gpt-5.4","input":"Reply with exactly: pong"}'
```

You can also call the bridge directly:

```console
$ curl -s localhost:8083/v1/responses \
    -H 'content-type: application/json' \
    -d '{"model":"gpt-5.4","input":"Reply with exactly: pong"}'
```

## Isolation

At startup the bridge resolves the executable, verifies that it identifies itself as `codex-cli`, checks for `Logged in using ChatGPT`, creates an empty read-only working directory, and discovers installed `SKILL.md` files so each can be explicitly disabled.

Each call uses `--ephemeral`, `--strict-config`, `--ignore-user-config`, `--ignore-rules`, a read-only sandbox, approval policy `never`, the OpenAI provider, and `personality="none"`. Tool, app, plugin, MCP, browser, image, shell, plan, user-input, hook, skill, and multi-agent surfaces are disabled. Configurable permission, environment, app, and collaboration instruction sections are omitted. The only request-specific developer instruction and user text are passed to the model. Codex's model-level base prompt remains unavoidable.

The child environment removes all `OPENAI_*`, `CONDUCTOR_*`, and `CODEX_*` variables, including `CODEX_HOME`. Codex therefore uses the normal login and skill locations under the user's `HOME` instead of inheriting an IDE or parent agent's private session.

Cancellation kills and reaps only that request's subprocess. JSON Schema files are request-scoped temporary files and are removed after the process exits.
