# Codex bridge implementation plan

## Objective

Add a development-only `codex-bridge` service that accepts the OpenAI Responses HTTP protocol and runs requests through a locally installed Codex CLI authenticated with ChatGPT. Dragoman should see the bridge as an ordinary `openai-responses` backend and remain unaware of Codex's private app-server protocol.

The first supported workload is a single request with no conversation history and no caller-defined tools. History, reasoning-state continuity, and external tools are follow-on capabilities.

## Intended architecture

```text
Responses client
    |
    | POST /responses (buffered or SSE)
    v
dragoman
    |
    | openai-responses; minimal pass-through
    v
codex-bridge :8083
    |
    | JSON-RPC over stdio
    v
codex app-server
    |
    | ChatGPT-authenticated Codex traffic
    v
OpenAI
```

`codex-bridge` owns all Codex-specific behavior:

- starting, monitoring, and stopping `codex app-server`;
- mapping Responses requests to app-server threads and turns;
- translating app-server notifications into Responses bodies and SSE events;
- isolating Codex from user and repository customization;
- preserving reasoning state and history when supplied;
- enforcing the deliberately limited initial feature set.

Dragoman uses its existing `openai-responses` dialect. A local service can be configured as:

```yaml
mode: append
codex:
  endpoint: http://127.0.0.1:8083
  protocol: openai-responses
  auth: CODEX_BRIDGE_KEY
```

`CODEX_BRIDGE_KEY` is a non-secret local sentinel required by Dragoman's current service availability check. The bridge ignores the resulting authorization header and must listen on loopback by default.

## Design principles

1. Preserve the incoming Responses document as raw JSON for as long as possible. Parse only fields needed to drive app-server.
2. Do not silently discard unsupported semantics. Return a clear `400` until a field is mapped and tested.
3. Prefer app-server's Responses-shaped primitives over reconstructing content from terminal prose.
4. Spawn a fresh app-server process and create one ephemeral Codex thread per HTTP request. The caller, not the bridge, owns conversation history.
5. Keep ChatGPT authentication separate from API-key authentication and fail closed if the active mode is not ChatGPT.
6. Treat prompt purity as a measured property. Capture what Codex sends rather than assuming a flag removed it.
7. Keep the bridge development-only, loopback-only, and unauthenticated beyond local process boundaries.
8. Keep no conversation or app-server state between HTTP requests. Process pooling is a future performance optimization, not part of the initial design.

## Phase 0: local CLI prerequisites

- Remove or bypass the stale npm/npx Codex launcher currently earlier on `PATH`.
- Resolve and pin the working Codex binary through a `--binary` flag. The currently working installation reports version `0.149.1`.
- Verify `codex --version`, `codex app-server --help`, and generated app-server schemas at startup or in a diagnostic command.
- Sign the working CLI in with ChatGPT and verify `codex login status` reports ChatGPT authentication. It currently reports `Not logged in`.
- Strip `OPENAI_API_KEY`, `CODEX_API_KEY`, and other API-provider overrides from the app-server child environment so a shell variable cannot silently switch the bridge to usage-based API billing.
- Add a startup refusal when authentication is absent, API-key based, or cannot be identified.

Do not copy, print, log, or expose the Codex authentication cache.

## Phase 1: measure prompt and tool purity

Before building the HTTP bridge, create a small protocol-capture harness:

1. Configure Codex with a temporary custom model provider pointing to a local capture server.
2. Start app-server with an isolated temporary `CODEX_HOME` and empty working directory.
3. Start an ephemeral thread and one trivial turn.
4. Capture the exact Responses request Codex would send upstream.
5. Return a minimal synthetic Responses answer so the turn completes without a paid model call.

Use the harness to measure the effect of:

- empty versus omitted `baseInstructions`;
- empty versus omitted `developerInstructions`;
- `personality: none`;
- an empty working directory;
- an isolated configuration home;
- disabled MCP servers, plugins, apps, skills, hooks, web search, multi-agent behavior, and memories;
- read-only sandboxing and `approvalPolicy: never`;
- ephemeral threads.

Record:

- all instruction messages sent upstream;
- every advertised tool and its schema;
- repository or user files reported in `instructionSources`;
- any metadata or context added by Codex;
- the exact model and reasoning parameters Codex selects.

Choose the smallest stable configuration that produces a useful request. Add an automated purity regression test around the captured request shape so a Codex upgrade cannot silently add instructions or tools.

## Phase 2: per-request app-server client and lifecycle

Create `cmd/codex-bridge` with a long-running HTTP listener analogous to `claude-bridge`. For each accepted `POST /responses`, spawn a new `codex app-server` child dedicated to that request. The child, its JSON-RPC connection, and its ephemeral thread live no longer than the HTTP request.

Per-request flow:

1. Start `codex app-server` over stdio with the isolated environment and empty working directory.
2. Complete the JSON-RPC `initialize` / `initialized` handshake.
3. Start one ephemeral thread.
4. Inject supplied Responses history, if any.
5. Start one turn and translate its notifications into the HTTP response.
6. On completion, cancellation, disconnect, or failure, close the JSON-RPC connection and terminate the child.

JSON-RPC is only the private transport used to control the temporary Codex child. It does not imply shared server state, persistent threads, or a bridge-side conversation store.

Implement:

- binary discovery and version reporting;
- child environment construction;
- per-request app-server startup over stdio;
- the JSON-RPC `initialize` / `initialized` handshake for each child;
- request IDs scoped to the child connection;
- response and notification dispatch for the request's single thread and turn;
- server-request replies for approvals and, later, tools;
- cancellation through `turn/interrupt` when the HTTP client disconnects;
- a short grace period followed by forced child termination if interruption does not stop it;
- reaping every child process on all success and error paths;
- graceful HTTP-server shutdown that cancels and reaps all active request children;
- `GET /health` reporting binary, version, and authentication readiness without starting a persistent app-server or exposing account secrets.

Concurrent HTTP requests use independent app-server processes. They share no JSON-RPC connection, thread, notification dispatcher, or model-visible history. Do not add a process pool until startup latency has been measured and the isolation trade-off is explicitly accepted.

Generate or check in only the small subset of app-server types the bridge uses. Do not hand-maintain the entire generated protocol schema.

## Phase 3: single-turn buffered Responses MVP

Support `POST /responses` with:

- string input;
- one user message containing text input;
- optional `instructions`;
- model selection when available to the signed-in Codex account;
- `reasoning.effort` and `reasoning.summary`;
- `text.format` JSON Schema through app-server `outputSchema`;
- buffered output;
- usage data when app-server reports it;
- cancellation when the caller closes the request.

For each request:

1. Validate the supported request shape.
2. Spawn and initialize a dedicated app-server child.
3. Start an ephemeral thread in the empty bridge working directory.
4. Assert that `instructionSources` is empty.
5. Map the request's instructions and current input using the purity configuration established in Phase 1.
6. Start one turn.
7. Collect completed response items and usage.
8. Return a valid Responses object using stable bridge-generated IDs where Codex does not provide an upstream ID.
9. Close the connection and reap the child. Explicit thread deletion is unnecessary because both the thread and process are ephemeral.

Initially reject with `400`:

- caller-defined tools or `tool_choice`;
- multiple historical input items;
- function-call outputs;
- unsupported modalities;
- parameters whose Codex semantics have not been verified;
- `store: true` if it would conflict with ephemeral execution.

The bridge must distinguish caller errors (`400`), unavailable or unauthenticated Codex (`503`), app-server/protocol failures (`502`), and cancellation.

## Phase 4: streaming

Add `stream: true` using app-server turn and item notifications.

- Emit a valid Responses SSE sequence with monotonically increasing sequence numbers.
- Map agent-message text deltas to `response.output_text.delta`.
- Map reasoning-summary deltas when requested.
- Use completed raw response items when available to retain IDs, summaries, and `encrypted_content`.
- End with a complete response carrying final status and usage.
- Ensure app-server progress, shell activity, internal tool calls, and diagnostics never leak as assistant text.
- Interrupt the Codex turn promptly on HTTP disconnect or write failure.

Buffered and streamed requests must fold to the same semantic response.

## Phase 5: caller-managed history and reasoning continuity

Do not make persistent Codex sessions the primary conversation mechanism. Reconstruct each request in a new ephemeral thread from the history supplied by the client.

App-server's `thread/inject_items` accepts raw Responses API items and places them directly in model-visible history. Use it to inject, in original order:

- user, assistant, developer, and system messages;
- reasoning items, including summaries and opaque `encrypted_content`;
- function calls and matching function-call outputs.

Dragoman's neutral item model already retains these core item types and encrypted reasoning state, but the Codex bridge should use the original raw input where possible to preserve fields added by newer Responses versions.

History algorithm:

1. Identify the final input segment that triggers the new generation.
2. Inject every preceding item verbatim with `thread/inject_items`.
3. Send the final user message through `turn/start`.
4. For a request ending in tool output rather than a user message, test whether injecting the output followed by `turn/start` with an empty input array continues generation without adding synthetic text.
5. Reject ambiguous layouts rather than guessing.

Tests must prove that encrypted reasoning blobs are byte-for-byte stable across response, client replay, injection, and the next generated turn. The bridge never attempts to inspect or manufacture hidden reasoning.

Persistent thread IDs may be added later as an optimization for `previous_response_id`, but only after stateless reconstruction is correct. The client-supplied history remains the source of truth.

## Phase 6: caller-defined tools

Treat tools as a separate milestone because app-server dynamic tools are experimental in Codex `0.149.1`.

Prototype the following mapping:

- Responses function definitions -> app-server `dynamicTools` on `thread/start`;
- app-server `item/tool/call` -> Responses `function_call` item and streaming events;
- later Responses `function_call_output` -> raw history injection and continued generation;
- tool selection modes -> Codex behavior only where equivalence is demonstrated.

There are two possible execution models:

1. **Caller loop:** return the function call, end or suspend the Codex turn, and continue when the caller posts the function output.
2. **Bridge callback:** keep the Codex turn open while an internal client executes the requested tool and sends the result back to app-server.

The public Responses contract requires the caller-loop model. Prove that a pending or interrupted dynamic-tool call can be resumed without duplicate model work before enabling it. If not, continue to reject caller tools rather than presenting bridge-executed tools as equivalent.

Do not expose Codex's built-in shell, filesystem, web, MCP, or other internal tools as caller-owned Responses function calls.

## Phase 7: integration and developer workflow

- Add `make codex-bridge` with configurable bridge address and binary path.
- Add a development service configuration example.
- Document ChatGPT login, plan allowance behavior, loopback-only operation, and the difference between Codex plan usage and API billing.
- Document that this is a Responses-shaped Codex agent endpoint, not a general OpenAI API replacement.
- Add the root build artifact `/codex-bridge` to `.gitignore` without hiding `cmd/codex-bridge`.
- Keep provider-backed live tests opt-in and clearly marked because they consume plan allowance.

## Testing strategy

### Unit tests

- Responses request validation and field mapping.
- App-server JSON-RPC framing, IDs, dispatch, and error handling.
- Notification-to-Responses event conversion.
- Buffered folding of the streaming event sequence.
- Child environment filtering.
- History partitioning and raw item injection.
- Reasoning summary and encrypted-content preservation.
- Cancellation, child termination, and process reaping.

### Deterministic integration tests

Use a fake app-server subprocess that speaks JSON-RPC and emits recorded notification sequences. Cover:

- successful buffered and streamed answers;
- reasoning followed by text;
- structured output;
- app-server refusal and crash;
- HTTP disconnect during a turn;
- concurrent requests using separate children with no event or history crossover;
- malformed and unknown notifications;
- history injection order;
- dynamic tool calls when that phase is enabled.

Use the custom-provider capture harness for prompt-purity regression tests.

### Optional live tests

Run only when explicitly enabled and ChatGPT authentication is confirmed:

- one minimal buffered request;
- one streamed request;
- structured output;
- history with returned encrypted reasoning state;
- account usage change visible through the supported Codex status surface.

Never make a paid or plan-consuming call during ordinary `go test ./...`.

## Acceptance criteria for the initial release

- Dragoman reaches the bridge through its unchanged `openai-responses` dialect.
- A supported single-turn request returns a valid buffered or streamed Responses answer.
- The app-server uses ChatGPT authentication and cannot inherit an API key accidentally.
- The bridge loads no repository instructions, user instructions, skills, plugins, MCP servers, hooks, or apps beyond the explicitly measured unavoidable Codex base behavior.
- Buffered and streamed output agree semantically.
- Cancellation stops the corresponding Codex turn and reaps its app-server child.
- Each concurrent request owns an independent app-server child and cannot receive another request's events or history.
- No app-server process or Codex thread survives its HTTP request.
- Unsupported history, tools, modalities, and parameters fail explicitly.
- Unit and deterministic integration tests run without authentication or plan usage.
- The bridge binds to loopback by default and is documented as development-only.

## Decisions to make after the purity spike

- Whether `baseInstructions` truly replaces Codex's built-in base prompt or only supplements it.
- Whether `developerInstructions` should carry Responses `instructions` or whether raw developer/system items are more faithful.
- Whether raw completed-response notifications expose every item and encrypted field needed for lossless output.
- Whether an empty-input turn correctly continues after injected function-call output.
- Whether the unavoidable Codex tool schema and prompt overhead are acceptable for the intended evaluations.
- Whether dynamic tools can support the caller-owned Responses tool loop without holding an HTTP request open indefinitely.
