---
slug: claude-cli-backend
type: feat
status: draft
created: 2026-07-27
supersedes: feat-protocol-translating-llm-proxy#out-of-scope/subprocess-backends
---

------

# Feature: Claude CLI as a backend

## Purpose

A backend, so far, is a dialect at a URL with a key in the environment. Some capacity is not
reachable that way. A developer with a Claude subscription has a locally installed, already
authenticated `claude` binary and no API key at all, and today dragoman cannot route a single
request through it.

This adds one backend that is a **process rather than an endpoint**. A request arrives in
`openai-responses`, is executed by spawning the local CLI, and comes back as
`openai-responses`. The caller learns nothing new: same input format, same output format, same
exit codes, same drop reporting. Which is the whole point — from the outside it is one more
value for `--backend`.

The parent spec put this out of scope, and gave a reason that has since been settled rather
than solved:

> Routing through a locally installed agent CLI is where this gets interesting, but such a
> harness resolves tool calls itself rather than returning them, and making it surface an
> unresolved call needs an interception mechanism that is not yet designed.

That interception mechanism is still not designed, and this feature does not design it.
It takes the other branch: **tool calling is not supported against this backend and is dropped
loudly.** What remains is one-shot execution, which is worth shipping on its own and is
honest about what it is not.

## What this backend can do

Verified by running the installed CLI, 2026-07-27, `claude` 2.1.220.

The envelope is: **any amount of context, exactly one question, no tools the caller owns.**

| capability | verdict |
|---|---|
| one-shot request → response | yes |
| streamed or buffered output | yes |
| `instructions`, `model`, `reasoning.effort` | yes |
| structured output (`text.format.schema`) | yes |
| reasoning output, including signatures | yes |
| provider state round-trip (`encrypted_content`) | yes, verified live |
| prior assistant turns, tool calls and results replayed as history | yes, within one turn |
| caller-declared tools | **no — dropped** |
| more than one user message in `input` | **no — exit `2`** |
| `max_output_tokens` | **no — dropped** |

One row is narrower than it looks, and building it is what narrowed it.

**A tool result is a user turn.** Replaying `[user, call, output]` — the ordinary
shape of a client continuing after it ran a tool — is two user messages to this
backend and is refused. Only a history whose sole user message is the last one
survives, which in practice means tool-history replay is refused too.

Provider state is the row worth having checked rather than assumed. A response
carrying a thinking signature was echoed back to the CLI unaltered and accepted,
and the model answered from the assistant turn it rode on — so a conversation
built out of this backend can be replayed to it, which is the only way state
survives a turn boundary when the proxy is stateless.

Two of those are not preferences and cannot be worked around.

**Tool declaration has no channel.** The CLI's `--tools` selects from its own built-in set, and
MCP tools are executed by the CLI itself. Nothing lets a caller declare a function and receive
a `tool_use` addressed back to them. Tools placed in the stdin envelope — top level and inside
`message`, in correct Anthropic `input_schema` form — are silently ignored: no error, and the
model states it has no tools. The parent spec's weather request, whose entire purpose is to
force a `get_weather` call, returns prose containing an invented seasonal temperature range for
Rio. That is the exact failure mode the drop machinery exists to prevent, which is why it must
be a named drop rather than an accepted request.

**Each user message on stdin fires its own model turn.** Sending `[user, assistant, user]`
produced two `result` frames, and the model's own reply to the first message entered the
history alongside the injected assistant turn. So a second user message does not cost extra —
it corrupts. Assistant turns inject cleanly; only user turns trigger.

The consequence is worth stating plainly, because it is the opposite of what the capability
list suggests at a glance: **a chat client does not work.** Turn two of any conversation is
`[user, assistant, user]`, which is the failing shape, so such a client breaks on its second
message and does so quietly. The clients this serves are one-shot ones — classify, extract,
summarise, translate, answer this one thing.

## Scope

### No new dialect

The CLI's output frames are `anthropic-messages` events verbatim, wrapped one level:

```json
{"type":"stream_event","event":{"type":"content_block_delta","index":0,
 "delta":{"type":"signature_delta","signature":"Ep0CCok…"}},"session_id":"…","uuid":"…"}
```

`message_start`, `content_block_start`/`_delta`/`_stop`, `message_delta` with usage,
`message_stop` — the whole grammar `anthropicmessages` already decodes, including thinking
blocks with signatures. Its input is likewise Anthropic messages, one per line, instead of a
`messages` array.

So this is **not** a fifth dialect. It is the `anthropic-messages` dialect reached over a
different transport, with a different capability set. Adding a dialect package here would
duplicate a decoder to gain nothing (R-2), and would want to import `anthropicmessages` to
avoid doing so — which the parent spec forbids outright. Neither is necessary.

The three things that actually differ are already modelled concepts:

| what differs | where it belongs |
|---|---|
| reached by spawning a process, not by POSTing | the executor |
| no tools, no output cap, one user turn | the backend's capability fields |
| frames arrive inside an envelope | the executor, on the way back |

### Selecting it

`--backend claude-cli` is sugar, and stays sugar, for:

```sh
dragoman --backend-protocol anthropic-messages --endpoint exec:claude < req.json
```

The `exec:` scheme on `--endpoint` is what selects the process transport, and its remainder is
the binary — a name resolved on `PATH`, or an absolute path. This keeps the parent spec's rule
intact: anything a preset does is expressible in flags, so a differently installed binary is a
command line rather than a patch. It also adds no flag, and the scheme is the natural place for
"where this backend is" to say "not over the network".

Executor selection is a dispatch table keyed on the scheme, not a type switch (R-4). `cmd/`
keeps owning argv, streams, and the exit code, and gains no knowledge of processes.

**The scheme carries the capabilities, not the service table.** Building it the other way
around produced a flag form that silently sent tools into a void while the table entry dropped
them — the same command, two behaviours, and the quiet one is the wrong answer this design
exists to prevent. What this backend cannot do follows from spawning an agent CLI rather than
from which entry named it, so `withSchemeLimits` applies it to any `exec:` endpoint and the
table entry states nothing beyond its endpoint and protocol. The parent suite's
`TestPresetsAreExpressibleInFlags` then covers the equivalence for free.

### Credentials: the backend authenticates itself

Every existing backend names an environment variable, and `Run` refuses to proceed when it is
empty. This one has no key — the CLI holds its own OAuth credential and reports
`"apiKeySource":"none"`.

So **an empty `KeyEnv` means the backend authenticates itself**, and the missing-credential
check applies only to backends that named a variable. That is the smallest honest change: it
does not weaken the check for anything that has a key.

Authentication is then verified rather than assumed, at preflight, because failing at spawn
time with a raw CLI error is exactly the opacity leak this feature is supposed to avoid.

### Preflight

Two checks, both before any request is translated or sent, both exit `2` — dragoman could not
run:

| check | mechanism | message |
|---|---|---|
| the binary exists | `exec.LookPath` on the `exec:` remainder | `claude-cli backend: "claude" not found on PATH; install Claude Code or pass --endpoint exec:/path/to/claude` |
| it is authenticated | `claude auth status --json` → `{"loggedIn": true, …}` | `claude-cli backend: not authenticated; run "claude auth login"` |

`auth status` exits `0` and prints JSON with `loggedIn`, so this costs no tokens and makes no
model call. Anything other than a parseable object with `loggedIn: true` is treated as not
authenticated; a CLI too old to have the subcommand fails the same way and says so, which is
the correct outcome either way.

Both are `2` rather than `1`: `1` means the backend said no, and a backend that is absent or
logged out never got asked.

They run at execution time, inside the executor, not before translation. `--no-execute` makes
no call and must keep working on a machine with no CLI installed at all — it is the whole
pipeline minus the network, and that is what makes it the mode most tests run in.

### Opacity is enforced, not offered

Left to its defaults the CLI is an agent in a working directory. It loads `CLAUDE.md`, runs
hooks, resolves skills and plugins, persists a session, and executes tools. A caller who sent a
Responses body did not ask for any of that, and got a wrong answer when it happened: with only
`--system-prompt` set, this repository's own `CLAUDE.md` reached the model and it answered
"I need to check for required reading first" instead of the question.

So dragoman always passes a fixed set, and none of it is configurable:

| flag | why |
|---|---|
| `-p` | non-interactive |
| `--output-format stream-json --verbose` | the only machine-readable form; `--verbose` is required with it |
| `--input-format stream-json` | the only form that accepts message history |
| `--include-partial-messages` | without it there are no incremental events, only whole messages |
| `--safe-mode` | suppresses `CLAUDE.md`, hooks, skills, plugins, MCP — and unlike `--bare` keeps OAuth |
| `--tools ""` | no tool execution |
| `--disable-slash-commands` | caller text is never interpreted as a command |
| `--strict-mcp-config` | no ambient MCP servers |
| `--no-session-persistence` | statelessness is the design; nothing is written |
| `--system-prompt <instructions or empty>` | see below |

`--system-prompt` is always passed, with the caller's `instructions` or with the empty string.
Omitting it leaves Claude Code's own agent prompt in place, which is a large body of meaning the
caller never asked for and which demonstrably changes the answer. Passing empty is not writing a
prompt — it is declining to add one, which is what *dragoman does not write prompts* requires
here. The empty value is accepted by the CLI.

`--bare` is the wrong instrument despite suppressing more: it forces `ANTHROPIC_API_KEY` and
never reads OAuth, so it removes the only reason to use this backend.

### Request mapping

The encoded `anthropic-messages` body already holds everything the CLI needs, so the transport
decomposes that one body rather than re-deriving anything from the base:

| body field | becomes |
|---|---|
| `model` | `--model` |
| `system` | `--system-prompt` |
| `output_config.effort` | `--effort` |
| `output_config.format.schema` | `--json-schema` |
| `messages[]` | one stdin line each: `{"type":"user"\|"assistant","message":{…}}` |
| `max_tokens` | nothing — dropped upstream |
| `tools`, `tool_choice` | nothing — dropped upstream |

Values stay opaque. `--effort` receives whatever arrived, and the CLI owns the set it accepts;
a level it rejects is the provider's refusal, reported as such.

This mapping is a pure function from a binary path and a body to argv plus stdin bytes. It
performs no IO and is table-tested without spawning anything.

### Streaming

There is no buffered form — the CLI only streams. That is the `StreamUpstream` quirk the
Anthropic backend already sets, and it is invisible to the caller for the same reason: folding
the translated stream is defined to produce exactly the buffered body. `--stream` and the
buffered default both work, and the fold invariant covers them.

The executor unwraps `stream_event` envelopes and passes `event` to the existing decoder. Frames
of other types — `system`, `assistant`, `user`, `rate_limit_event`, hook events — are the CLI's
own bookkeeping, not model output, and are discarded rather than decoded. The one exception is
the terminal `result` frame, read for errors.

### Errors

The CLI reports failure in-band, in its final frame, rather than in an exit status:

```json
{"is_error":true,"api_error_status":404,"result":"There's an issue with the selected model…"}
```

`is_error: true` is exit `1` — the backend said no — carrying `api_error_status` and `result` as
the message. A non-zero process exit or an unparseable stream with no `result` frame is also
`1`, since the backend was reached and did not answer. Preflight failures stay `2`.

### Drops

Three, all `unsupported` against this backend and none of them against the `anthropic-messages`
dialect, which supports all three. That is the distinction the capability fields exist for: it
is the endpoint that cannot do these, not the wire shape.

| dropped | path |
|---|---|
| tool definitions | `tools[n]` |
| forced or restricted tool choice | `tool_choice` |
| output cap | `max_output_tokens` |

Default: the response is returned and drops go to stderr. `--strict`: exit `2`, nothing sent.
For a tool-using client pointed at this backend, `--strict` is the correct posture and the drop
is the whole warning.

### More than one user message is an error, not a drop

Drops are field-level; no item type is ever dropped. Silently discarding a user turn would
change the meaning of the conversation, and silently *sending* it fires an extra billed turn
whose reply contaminates the history. Both are worse than refusing.

So a request carrying more than one user turn exits `2`:

```
claude-cli executes one turn per request; input has 2 user turns, at input[0], input[2]
```

It names the paths, like a drop does, because that is the caller's only signal.

What counts is a **turn**, not an item: consecutive items on the same side collapse into one
message, exactly as the encoder groups them, so the count is the number of turns the backend
would actually run. The rule lives in one function that both the encoder and the check call, so
what is grouped and what is counted cannot drift apart. Instructions and reasoning items open no
turn; a function output opens a user turn, because to the CLI a tool result is a user message.

## Technical requirements

- **The executor is the seam, and it already exists.** `Executor` is an interface with one HTTP
  implementation, kept that way so a non-HTTP backend is additive. This adds the second
  implementation and changes no signature in `core`.
- **Capability differences are data.** Tools, tool choice, output cap and the single-turn rule
  are fields on `base.Backend` consulted during encode, in the same way `NoSchema` already is
  for DeepSeek. The dialect gains no branch that names this backend.
- **`NoOutputCap` suppresses the protocol's own cap**, so the `anthropic-messages` default of
  20000 cannot fire here. There is nowhere to put a cap, and inventing one would be inventing it
  into a void.
- **Nothing in `cmd/` learns about processes.** Executor selection resolves from the backend
  record through a table, so the CLI stops naming an executor at all.
- **The argv-and-stdin mapping is pure** and lives beside the executor that uses it, as the
  HTTP executor lives beside the interface it implements.
- **Frame filtering is total.** An unrecognised CLI frame type is discarded, not an error: the
  CLI adds frame types between releases and a new one must not break a translation.

## Out of scope

- **Tool calling, in any emulated form.** Not via an MCP server dragoman hosts, not by
  describing the tools in the system prompt, not by parsing a call back out of prose. The first
  makes dragoman execute tools, the second writes prompts, the third invents structure from
  meaning. Each is forbidden by a constraint the parent spec states outright.
- **Multi-turn conversations.** Blocked on finding a stdin envelope that adds a user turn to
  history without triggering execution. The format is undocumented beyond the flag table, and
  only `type: "user"` and `type: "assistant"` were tested; if such a form exists this becomes a
  drop-nothing backend and the restriction lifts without any other change.
- **`--resume` / `--continue` / session reuse.** Server-side history is what statelessness
  declines, here as everywhere.
- **`--bare` and API-key mode.** It forecloses the subscription this exists to reach.
- **Other agent CLIs.** The `exec:` scheme and the executor table make a second one cheap, but
  nothing here is designed to be shared with one yet.
- **Image input.** The block shape is Anthropic-native and should pass, but it was not run and
  the parent spec excludes images anyway.
- **Cost reporting.** The `result` frame carries `total_cost_usd`; nothing consumes it, in
  keeping with the parent spec's refusal to compute or cost usage.

## Constraints

- Bound by `CONSTITUTION.md` and by every constraint in the parent spec, which this one narrows
  rather than relaxes.
- **No new dependency.** `os/exec` and `encoding/json`.
- **The subprocess is killed with its context, and the reader stopping counts as cancellation.**
  This is the failure a subprocess backend has that an HTTP one cannot, and it was found by
  building it: `StdoutPipe` documents that `Wait` must not be called before the pipe is drained,
  and the reader has two early returns that leave it undrained — a frame the dialect cannot
  decode, and a stdout that will not take an event. The child then blocks on a full pipe, `Wait`
  blocks on the child, and dragoman hangs *instead of printing the diagnostic it was about to
  print* — strictly worse than the error it was reporting, with a `claude` stranded behind it. So
  the executor owns a cancellable context, cancels it whenever the read ended early, and sets a
  `WaitDelay` as a backstop against a killed child leaving a pipe open. A regression test drives a
  stub that emits an undecodable frame and then writes more than a pipe buffer holds.
- **Nothing is written to the user's filesystem** by a dragoman invocation:
  `--no-session-persistence` is load-bearing, not hygiene.
- Secrets stay out of argv. Not a new burden — there is no secret to pass — but the request body
  reaches the CLI on stdin rather than in an argument, and the prompt is a caller's data.

## Done criteria

### Offline — no binary, no network

The bulk of the suite. The CLI's frames are recorded to
`testdata/fixtures/claude-cli/stream.ndjson` from a live run and replayed.

| check | expected |
|---|---|
| argv and stdin built from a `anthropic-messages` body | flags as tabled, one NDJSON line per message |
| a body carrying `instructions` | `--system-prompt` with that text |
| a body carrying none | `--system-prompt` with the empty string, still passed |
| effort and schema present | `--effort` and `--json-schema` set, values byte-identical |
| recorded frames decoded | a well-formed response carrying the CLI's answer and usage |
| fold(translated stream) | equals the translated buffered response |
| a recording carrying a thinking signature | the pocket is filled, and echoing it back reproduces the signature byte for byte on the assistant turn |
| a request carrying tools | `tools[0]` and `tool_choice` dropped, reason `unsupported` |
| the same request under `--strict` | exit `2`, names both paths |
| a request carrying `max_output_tokens` | dropped; no cap invented, the protocol default does not fire |
| a request with two user turns | exit `2`, names both paths |
| a request with one user message and three assistant items | accepted |
| a replayed tool exchange whose causing question is present | exit `2`: a tool result is a user turn |
| the same exchange without that question | accepted |
| a table-named and a flag-named `exec:` backend | identical capabilities, so identical behaviour |
| an unrecognised CLI frame type in the stream | discarded, no error |
| a stream whose final frame is `is_error: true` | exit `1`, message carries `api_error_status` |
| the fixed flag set | asserted in full, values included — `--tools ""` and `--tools all` are the same flag and opposite behaviours |
| `--no-execute` | prints the body the invocation is built from, spawns nothing |

### Against a stub binary — no CLI installed

A real process, spawned the real way, so the executor is exercised rather than replaced.

| check | expected |
|---|---|
| a stub that answers | exit `0`, the recorded answer |
| a stub reporting `loggedIn: false`, or unreadable status | exit `2` |
| an endpoint naming a binary that is not there | exit `2` |
| a stub that exits non-zero | exit `1`, carrying its stderr |
| a stub that ends without a result frame | exit `1` |
| a stub that never reads a stdin larger than a pipe buffer | exit `0` — no deadlock |
| a stub emitting an undecodable frame, then flooding stdout | the decode error, not a hang |

### Preflight

| check | expected |
|---|---|
| `exec:` naming a binary that does not exist | exit `2`, names the binary, suggests install |
| `auth status` reporting `loggedIn: false` | exit `2`, suggests `claude auth login` |
| `auth status` unparseable or subcommand absent | exit `2`, treated as not authenticated |
| `--no-execute` on a machine with no CLI | translates and prints, never spawns, never fails |

### Live — the case this feature exists for

Skipped, not failed, when preflight does not pass; this suite must stay runnable by someone
without Claude Code installed.

**The headline criterion is a plain knowledge question, answered.** One request in
`openai-responses` form carrying nothing but a question a model can answer from what it knows —
`testdata/knowledge.json`, no tools, no schema, no history:

```sh
dragoman --backend claude-cli --model sonnet < testdata/knowledge.json
```

returns a well-formed `openai-responses` body containing the answer, exit `0`. Nothing about the
output distinguishes it from the same question routed to `--backend anthropic`, which is the
entire claim this feature makes: **one more value for `--backend`, and no local key.**

That single command passing is the definition of done. The parent spec's live matrix leads with
a tool call because that was the interesting case there; here the interesting case is that the
boring one works at all, over a transport that is a process rather than a URL and with no
credential in the environment.

The rest of the matrix qualifies it:

| check | expected |
|---|---|
| the same question, `--backend anthropic` | the same shape of body, so the two are interchangeable to a client |
| a one-shot request, buffered | a well-formed `openai-responses` body |
| the same request, `--stream` | events on stdout; the fold equals the buffered body |
| a request whose answer requires reasoning | a reasoning item with `encrypted_content` populated |
| an assistant history carrying a thinking signature, echoed back | accepted, no error, and answered from that turn — provider state survives the turn boundary |
| a request carrying `text.format.schema` | the response parses against the schema |
| `testdata/weather.json` unmodified | exit `2` under `--strict`; prose and a stderr drop without it |

The last row is the one that matters most. The parent spec's live matrix is built on that
request, and this backend is the first that cannot serve it. The suite should say so in the
same words the caller would see.

## Open questions

- **Environment context still leaks.** With `--safe-mode` and an empty `--system-prompt`, the
  model still knew the current date, so the CLI injects some machine context dragoman does not
  control. `--exclude-dynamic-system-prompt-sections` is documented as ignored when
  `--system-prompt` is set, so there may be no lever. It is small, but "opaque" is the claim
  being made, and the gap between the claim and the truth should be measured before shipping
  rather than discovered.
- **`--safe-mode` is the whole opacity guarantee, and it is one flag deep.** If a future release
  changes what it suppresses, requests silently start carrying a user's `CLAUDE.md` again. Worth
  a test that asserts the absence of local context in a live answer, which is an awkward thing
  to assert and may be worth it anyway.
