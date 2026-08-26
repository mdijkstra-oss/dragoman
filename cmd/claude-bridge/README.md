# claude-bridge

A small HTTP server that speaks the anthropic-messages wire format on localhost and answers every request by spawning the locally installed `claude` CLI (Claude Code). A dragoman service pointed at it bills the Claude subscription account that signed in to Claude Code, instead of an API key — so a developer can run prompts and evals nearly as they would run against the real endpoint, without spending API credit.

> [!WARNING]
> Development tool only. Not a production backend. Whether your Anthropic plan permits this use is between you and Anthropic; claude-bridge does not settle it.

> [!CAUTION]
> This is not the real endpoint. Limits:
> - **No tool calling.** The CLI resolves its own tools and has no channel to return a tool call to the caller. A request carrying `tools` or `tool_choice` is refused with `400`. Agents that loop over tool calls cannot run through the bridge.
> - **One turn per request.** The CLI runs one turn per user message. More than one user turn, or a tool_use/tool_result exchange in the history, is refused with `400`. One user turn preceded by assistant turns is accepted.
> - **Structured output is near-native.** It comes back through the CLI's `--json-schema` as a `StructuredOutput` tool_use block. dragoman already folds that into message text, so the client sees JSON text as it would from the real endpoint.
> - **The prompt is close, not identical.** `system` is passed via `--system-prompt`, but the CLI still wraps it in a short prompt of its own — measured at around 750 input tokens on a near-empty request, against a handful from the API. Answers are close; token counts run higher.
> - **Streaming only.** `stream: true` is required. dragoman always streams to anthropic-messages services, but a buffered request still gets `400`.
> - **Anthropic models only** — and only the ones the signed-in account may use.
> - **`max_tokens` is ignored.** The CLI has no flag for it.

## Run

Requires Claude Code on `PATH` and a signed-in account (`claude auth login`).

```console
$ go run ./cmd/claude-bridge
$ make bridge
```

On start it logs the listen address, the resolved binary path, and the signed-in account (email and subscription type).

| flag | default | does |
|---|---|---|
| `--addr` | `127.0.0.1:8082` | Address to listen on. |
| `--binary` | `claude` | The claude executable: a name on `PATH` or a path. |
| `-v`, `--verbose` | off | Log every frame the CLI emits. |

`GET /health` returns `{"ok":true,"account":"..."}`.

There is no client authentication. Keep it on loopback.

## Point dragoman at it

Override the built-in `anthropic` service so the client's request shape does not change:

```yaml
mode: override
anthropic:
  endpoint: http://127.0.0.1:8082/v1
  protocol: anthropic-messages
  auth: CLAUDE_BRIDGE_KEY
```

dragoman refuses to call a service whose named key variable is unset, so `CLAUDE_BRIDGE_KEY` has to be set to something — the bridge never reads its value.

```console
$ CLAUDE_BRIDGE_KEY=x dragoman serve --config bridge.yaml

$ curl localhost:8080/responses -d '{"model":"anthropic/claude-sonnet-5","input":"Reply with exactly: pong"}'
```

`serve` resolves the service from the model's first segment. The same config works over a pipe:

```console
$ echo '{"input":"Say hello."}' | dragoman --config bridge.yaml --backend anthropic
```

## The account that answers

The CLI prefers an API key in its environment over its sign-in. claude-bridge strips every `ANTHROPIC_*` variable, plus `CLAUDE_CODE_USE_BEDROCK` and `CLAUDE_CODE_USE_VERTEX`, from the child's environment before it runs, so a key inherited from a shell profile, a `.env` file or a compose file cannot silently move the call — and the bill — to another account. `CLAUDE_CONFIG_DIR` is kept, because it says where the sign-in lives.

Each call runs from an empty temporary directory with `--safe-mode`, `--tools ""`, `--strict-mcp-config`, `--no-session-persistence` and `--exclude-dynamic-system-prompt-sections`, so no CLAUDE.md, settings, hooks, skills or MCP servers from the working tree reach the model.

## How a request travels

```
client --openai-responses--> dragoman --anthropic-messages--> claude-bridge --stream-json--> claude CLI
```

dragoman translates exactly as it would for the real endpoint. claude-bridge reads the anthropic-messages body only for `model`, `system`, `output_config` (`effort`, `format.schema`) and `messages`, and passes them on as CLI flags and stream-json stdin lines.

The CLI's `stream_event` frames carry anthropic-messages events verbatim. claude-bridge re-emits them as server-sent events (`event:` / `data:`), and dragoman decodes them with the same dialect it uses for `api.anthropic.com`.

CLI failures arrive in the result frame, not in the exit status. A failure before the first event is reported as `HTTP 502` with the CLI's message; a failure after the first event arrives as an `error` event in the stream.
