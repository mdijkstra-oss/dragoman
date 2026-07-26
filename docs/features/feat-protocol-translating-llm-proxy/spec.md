---
slug: protocol-translating-llm-proxy
type: feat
status: draft
created: 2026-07-26
---

------

# Feature: Protocol translating LLM proxy

## Purpose

Every LLM client speaks exactly one dialect. An OpenAI SDK sends a Responses body and
parses a Responses body back; hand it an Anthropic response and it breaks. Every provider
speaks its own. So the mapping between them gets written again inside every application
that wants to change providers, and it gets written badly, because it is nobody's actual
product.

dragoman moves that mapping out of the application. A request goes in written in
`openai-responses`, gets executed against a provider speaking another dialect, and comes
back as `openai-responses`. The client never learns which provider answered.

It is stateless, and that is the whole design. One request in, one response out. No
session, no stored conversation, no agent loop, no tools executed on the caller's behalf.
Everything needed to answer is in the body that arrived, which is what makes the binary
interchangeable with the endpoint it stands in front of.

It is also deliberately dumb: dragoman moves structure and never meaning. Values pass
through untranslated, prompts are never written, capabilities are never emulated — every
rule below is that one rule in different clothes, because a proxy that starts being clever
fails quietly instead of loudly.

## Scope

### Dialects

The application speaks `openai-responses` to dragoman and receives `openai-responses`
back. Four dialects are available as a backend:

| value | wire |
|---|---|
| `openai-responses` | `POST /v1/responses` |
| `openai-completions` | `POST /v1/chat/completions` — also OpenRouter, DeepSeek, and every other compatible endpoint |
| `anthropic-messages` | `POST /v1/messages` |
| `gemini-interactions` | `POST /v1beta/interactions`, with header `Api-Revision: 2026-05-20` |

A dialect is a wire shape, not a vendor. `openai-completions` names a body format that
several companies serve. This distinction is the product, so the vocabulary has to carry
it — a value set mixing `completions` with `gemini` would blur the two things dragoman
exists to keep apart.

The two OpenAI dialects are why vendor is the wrong axis. They diverge on at least four
points: the tool schema is flat in one and nested in the other, structured output nests
`strict` at different depths, forcing a named function has a different shape, and only
Responses has a reasoning item at all.

`--protocol` accepts `openai-responses` and nothing else. It exists so that adding a
second external dialect later does not change the shape of the command line.

### A backend is a dialect plus a quirks record

Sharing a wire shape does not make two endpoints identical. DeepSeek serves
`openai-completions` and additionally: has no `/v1` in its base URL, takes a non-OpenAI
top-level `thinking` object, restricts `reasoning_effort` to `high|max`, has no
`json_schema`, emits a `finish_reason` value OpenAI does not define, silently ignores
sampling parameters in thinking mode, and returns 400 unless `reasoning_content` is echoed
back once a tool call has occurred.

Those are data, not code — a record in the preset table beside the endpoint and dialect.
OpenRouter's record is empty, which is the property the abstraction protects.

### The base format

**The base is `openai-responses`.** Not a format modelled on it — the same format. There is
no internal dialect and no neutral intermediate.

Translation therefore runs in exactly one direction per side:

- dialect packages **encode requests out of** the base, toward a backend
- dialect packages **decode responses into** the base, from a backend

Nothing encodes a response out of the base and nothing decodes a request into it, because
on both of those edges the base and the wire format are the same bytes.

**No dialect package may import another.** If two ever reference each other the design has
become N×M and the next dialect costs what all the previous ones cost combined.

The base adds one thing Responses does not have: **provider state, carried per item.** Every
provider returns something on a reasoning item that it wants handed back unaltered, and each
binds it to the item it arrived on rather than to the conversation. So this is a field on an
item, never a bag on the document.

The consequence to accept: the base tracks OpenAI's release cadence. dragoman owns the set
of dialects; OpenAI owns the base.

### Selecting a backend

A backend is a dialect at a URL with a key in the environment:

```sh
dragoman --backend-protocol gemini-interactions \
         --endpoint https://generativelanguage.googleapis.com/v1beta \
         --model gemini-3.6-flash < req.json
```

`--backend` is sugar that fills those from a table of presets. **The presets are a
convenience and must stay one**: anything a preset does has to be expressible in flags, so
that an endpoint nobody anticipated is a command line rather than a patch. The preset entry
also carries the backend's quirks record.

Credentials come from the environment only, never from a flag. argv is visible in `ps`, in
shell history, and in CI logs, and a secret that reaches any of those is already leaked.

### Model and effort

Both are carried in the request body by all four dialects, and both name something that
belongs to the provider rather than to dragoman.

The **field** is structural, and dragoman maps it. Each dialect keeps the model and the
reasoning level somewhere different — a top-level key in some, nested two or three deep in
others, under a different name for the same idea in each.

The **value** is opaque. It passes through byte for byte: never normalised, never
case-folded, never validated against a set, never mapped onto a nearer neighbour. `high`
goes out as `high`. This is not a simplification to be improved on later — reasoning levels
do not denote comparable strength across providers, and they do not denote it across models
within one provider either, so any mapping dragoman invented would be a guess wearing the
costume of a conversion.

> dragoman owns the set of dialects. Providers own the set of models and levels.

An unknown dialect panics (R-12); an unknown level is not dragoman's business. Neither model
nor level is modelled as a typed enum — a deliberate, narrow exception to R-13, because
encoding a provider's value space as a Go type means cutting a release every time that
provider adds a value it already accepts.

A backend that will not take the level it was handed rejects it in its own words, and
dragoman exits `1`. A backend that quietly reinterprets it does so server-side, invisibly to
everyone; that is the provider's business too.

Precedence is the `--model`/`--effort` flag, then the value in the body, then nothing.

**Absent must stay absent.** Providers default differently, so filling in a level nobody
asked for silently overrides a provider default and changes both cost and behaviour. If no
level arrives, the field is omitted.

One thing the dialect alone does not settle: **a field's home is occasionally
model-dependent.** Providers move reasoning controls between model generations and reject
on the older name. The value still passes through untouched; which field it lands in belongs
to the quirks record.

Nothing infers that a model name is wrong for a backend. The tempting rule — differing
dialects imply a wrong model, so fail before spending a request — is false in exactly the
case that matters: OpenRouter takes `anthropic/claude-sonnet-4` over `openai-completions`,
and Gemini serves an OpenAI-compatible endpoint too. Passing through and letting the
provider reject is both simpler and correct.

### Required fields the base does not carry

`anthropic-messages` is the only backend that **requires** an output cap — `model`,
`messages` and `max_tokens` are the three parameters it does not mark optional. The other
three treat it as optional with a model-dependent default.

So a Responses request carrying no output cap cannot be encoded for Anthropic without
inventing one, which *absent must stay absent* otherwise forbids. That splits into two
mechanisms, and they are not interchangeable:

- **`--max-tokens` is a flag**, with the same precedence as `--model` and `--effort`: flag,
  then the value in the body, then nothing. Like them it applies to whichever backend is
  selected. It sets a field; it does not exist to paper over a gap.
- **The fallback lives in the quirks record**, per backend, and fires only when a backend
  requires a field that neither flag nor body supplied. No other backend ever receives an
  invented value, so *absent stays absent* holds everywhere it still can.

Anthropic's fallback is **20000**. A cap that has to be guessed should err toward not being
the thing that ruins the answer, and on a reasoning model the cap covers thinking and output
together, so a small one truncates mid-thought. The consequence to size for: a non-streaming
request may legitimately run long, and dragoman's HTTP client timeout has to allow it.

A backend that requires a field with no flag, no body value, and no fallback exits `2`
naming the field. That branch is currently unreachable, and exists so the next such field is
a table entry rather than a discovery.

### Tool calls

Every dialect has both a call and a result, so **tool calls and results are never dropped.**
Both directions are exercised: the call comes back from the backend, the result goes out
with the next request.

The divergence is shape, not semantics. The JSON schema sits at a different depth in each
dialect — the two OpenAI dialects disagree with each other, and Gemini happens to match
Responses. The call id has a different field name at each end of each dialect. Anthropic
alone nests the result inside a user message rather than making it a sibling item. The base
holds one call identity and each dialect places it where it belongs.

Three things are not visible from the shapes and are worth naming, because they are traps
rather than lookups:

- **Arguments are a JSON string everywhere except `gemini-interactions`**, which carries a
  decoded object on a buffered call. Encoding parses; decoding serialises.
- **Forcing a named function differs in all four**, including between the two OpenAI
  dialects: one names the function directly, one wraps it a level deeper, Anthropic uses its
  own discriminator, and Gemini expresses it as a mode plus a single-element allow-list.
- **Responses returns two ids on a call** and the result references only one of them.

dragoman never executes a tool. A tool call is returned to the caller exactly as the
provider emitted it, because resolving it is the caller's job and holding it open would
require the state this design does not have.

### Structured output

Every dialect has it, under a different key, with the strictness flag at a different depth
— and the two OpenAI dialects again disagree with each other. Schema and tools may be
combined in one request against all four.

Two hazards:

- **Strictness granularity differs.** OpenAI's is per tool (`strict: true` on each function
  definition); Gemini's is per request (`tool_choice: "validated"`, which is `auto` plus
  constrained decoding — identical routing, added schema adherence). A request with some
  tools strict and some not cannot be expressed against Gemini, and is a named drop.
- **DeepSeek has no schema mode**, only unconstrained JSON — and that mode is driven from
  the prompt rather than from a parameter: the word "json" and an example of the shape have
  to appear in the system or user message, or the API may return empty content. Since
  dragoman does not write prompts, a schema against that backend is dropped and reported,
  and the caller gets prose. Putting the shape in their own prompt is what they would do
  without dragoman anyway.

### Streaming

`stream` is an **output** setting. It decides what the caller receives, not how dragoman
talks to the backend:

- **default — buffered.** One `openai-responses` body on stdout, written once, when the
  answer is complete.
- **`stream: true` in the body, or `--stream`** — the `openai-responses` event sequence, one
  event per line, so a client can render the answer as it is produced.

All four dialects carry `stream` in the body, so dragoman honours what arrived;
`--stream`/`--no-stream` exist only to force it from a shell without editing JSON.

There is **one event vocabulary and no intermediate verbosity level.** dragoman does not
invent an event grammar, so there is no output *format* to choose between — only whether the
one grammar is delivered folded or unfolded. A client wanting item-level progress without a
typing effect ignores `*.delta` events, which is a one-line filter at its end and a policy
decision dragoman has no business making.

What is emitted is the **complete Responses lifecycle, not the deltas alone** — item added,
content part added, deltas, part done, item done, response completed — carrying the
`item_id`, `output_index`, `content_index` and `sequence_number` the emitter mints. A stock
client reconstructs the answer from that sequence the way it always has, and renders the
typing itself. dragoman never renders, concatenates, or summarises; a mode that emitted only
text deltas would be prettier at a terminal and useless to the SDK this exists to serve.

Only framing varies, and framing belongs to the transport: newline-delimited JSON on stdout.
The bar is that re-framing the identical sequence as SSE — which is exactly what the HTTP
transport will do — is consumable by an unmodified SDK stream parser. So NDJSON is a
framing choice for a pipe, never reduced fidelity.

**Streamed output is written and flushed per event.** Accumulating events and writing them
at the end yields byte-identical stdout while defeating the whole purpose of the mode, so
this is a correctness requirement and not a performance note. It is the transport's job; the
core yields events and never decides when they reach a file descriptor.

The two axes are independent, and the fold invariant is what makes separating them safe:
dragoman may stream from a backend and still hand the caller one buffered body, because
folding that stream is *defined* to produce exactly that body. Sometimes it must — Anthropic
documents that at large `max_tokens` values its own SDKs "require streaming to avoid HTTP
timeouts", and the 20000 fallback above is such a value. Whether a backend is always
streamed upstream is therefore a quirks-record entry, invisible to the caller either way.

The four grammars do not align, and one difference drives the whole decoder design:

| dialect | item boundaries |
|---|---|
| `openai-responses` | explicit, item-scoped |
| `anthropic-messages` | explicit, block-indexed |
| `gemini-interactions` | explicit, step-scoped |
| `openai-completions` | **implicit** — inferred from which delta key is populated, keyed by `index`, closed only by the terminal chunk |

Three dialects announce when an item opens and closes. Chat Completions does not: a new tool
call is a new `index` appearing, and it ends when the stream says the turn is over. That is
the one decoder that has to infer structure rather than read it.

All four stream text and tool arguments incrementally, and all four stream reasoning where
they have it. Everything else — event names, delta key names, how streaming is requested —
is per-dialect detail to look up while writing the decoder, not to fix here.

One invariant keeps the two paths honest:

> **Folding a translated event stream must equal the translated buffered response**, for
> every fixture, for every backend.

### Dropped fields

Decoding parses into the typed base while retaining the raw JSON beside it. A drop is any
key present in the raw body that no typed field consumed — computed, never hand-maintained,
so an unmapped field is loud by default and adding support for it means adding a typed field
and watching it stop appearing.

Two reasons, because they have opposite lifetimes:

- **Unsupported** — the target dialect has no equivalent and never will. `logit_bias`
  against Anthropic, `cache_control` against anything, `safetySettings` against non-Gemini.
- **Unimplemented** — dragoman could map it and does not yet. Without the distinction the
  backlog is invisible inside a list of permanent impossibilities.

The diff cannot tell them apart; the dialect table says which.

Drops are **field-level only**. No item type is ever dropped: tool calls and results exist
in all four dialects, and a reasoning item only ever reaches a backend that produced it.

Entries carry a **path, not a name** — `input[3].content[1].signature`, not `signature` —
because skip-and-continue makes this the caller's only signal.

Behaviour:

- default — the response is returned, drops are written to stderr
- `--strict` — any drop is an error, exit `2`, nothing is sent

Silence is not an option. A dropped field produces a confident, well-formed, wrong answer,
and that failure is invisible precisely because the output still parses.

### Transports

One transport — **pipe**: stdin to stdout, or `-f`/`-o`. An HTTP listener is next and is out
of scope here.

The seam is the deliverable, not the second transport. **The core takes a request and returns
a response or an event sequence, and never touches `os.Stdout`.** A core that cannot tell
what invoked it is testable without a terminal, and the listener that comes later is an
addition rather than a refactor.

So the CLI owns argv parsing, reading and writing the streams, event framing, and the exit
code — and owns nothing else. Flag precedence, backend selection, translation, and the
dropped-field diff are core concerns the CLI hands over, never implements. Drops are returned
as data and the transport decides they go to stderr. Anything under `cmd/` that a second
transport would have to duplicate is in the wrong package.

### Diagnostics

- `--no-execute` — translate the request, print what would have been sent, make no call.
  This is the whole pipeline minus the network, so it is the mode most tests run in.
- `--raw` — return the backend's untranslated response. This is how a mapper bug is told
  apart from a provider quirk.
- `-v` — the translation and its dropped fields to stderr.

### Exit codes

- `0` — a response was returned and translated
- `1` — the backend said no: HTTP 4xx/5xx, or a provider error body
- `2` — dragoman could not run: unparseable input, unknown dialect, missing key, bad flags,
  a required field with no source, or a drop under `--strict`

`2` is reserved for "the tool could not run", which is the only thing that makes it
trustworthy in a hook or a CI step.

## Technical requirements

### Translation is a transducer

Per dialect, the stream decoder is a pure state transition and a flush:

```go
func Step(s State, in DialectEvent) (State, []Event, error)
func Flush(s State) ([]Event, error)
```

State is threaded, never mutated (R-9). Neither performs IO, so both are table-testable
without a network or a key (R-4). The transport picks its own surface — `iter.Seq2`,
channel, callback — and the core never learns which.

`[]Event` rather than one event, because the mapping is not 1:1: an Anthropic
`content_block_start` yields both an item-added and a content-part-added, and one Gemini
chunk with three parts yields three or more. Length is usually zero or one.

The machine's control is finite even though its registers are not. A block's type is fixed
at open and never changes in any of the four dialects, so no lookahead is required — emit on
close, never backtrack. What must be held is bounded by one item: an open-item map keyed by
index, because items interleave (Chat Completions identifies tool-call fragments by `index`
alone), and a per-item argument accumulator.

`Flush` exists for truncation. A stream may end with items still open — `max_tokens`, a
dropped connection, a mid-stream refusal — and without an explicit flush that is undefined
behaviour. Terminal state also arrives in different places: Anthropic's `stop_reason` on
`message_delta`, Responses' on `response.completed`/`.failed`/`.incomplete`.

Dialect selection is a dispatch table, not a type switch (R-4).

### One emitter mints Responses bookkeeping

Responses events carry `item_id`, `output_index`, `content_index` and `sequence_number`
that the source dialects do not. Those are minted, not translated.

Because there is one output grammar, that bookkeeping is written once and every decoder
drives it:

```go
func OpenItem(s State, k ItemKind) (State, []Event)
func TextDelta(s State, text string) (State, []Event)
func CloseItem(s State) (State, []Event)
```

Same shape as `Step`, state threaded not mutated. Each dialect decoder shrinks to
recognising what happened and calling in.

### Buffered derives from the streaming mapping

The mapping is written once. Each backend's buffered body is parsed into its logical pieces
and fed through the same `Step`; the per-dialect difference is only *how pieces are
extracted from a body*. Every translation decision stays in one place, and the fold
invariant guards it.

### Provider state rides in the reasoning item

Reasoning is a first-class item in every dialect, with the same two-part shape everywhere:
an opaque blob the provider wants back, and a human-readable summary it does not. The
encrypted half is unremarkable — all of them have one, Responses included. So reasoning is
a typed item with two fields, not a passthrough bag. `openai-completions` is the only
dialect without it, and DeepSeek supplies its own there.

**One rule covers all of them: whatever a backend returned as state goes back to that
backend unaltered and in position.** dragoman does not inspect it, does not classify it,
and does not care that one provider's is base64 and another's is readable prose. Providers
word the requirement differently — resend every step exactly as received, resend it in every
subsequent turn, never modify a signed block — and faithful round-tripping satisfies all of
them without any of them being modelled.

**A conversation lives on one backend for its whole life.** Histories are not interchangeable
between providers and never could be: encrypted state from one is meaningless to another, and
each provider built its own history in its own shape. So there is no cross-provider replay to
defend against — no origin tag, no rule about stripping foreign state, no retention table.

Some backends offer server-side history instead, keyed by a previous-response id. dragoman
does not use it; statelessness is the design, and every backend request is the full-history
form.

Because dragoman is stateless, that state survives a turn boundary only in the client's
hands. **It rides in the reasoning item's `encrypted_content`** and is pulled back out and
restored on the next request. Echoing encrypted reasoning items back is that field's
documented purpose, so clients preserve it verbatim; foreign bytes ride the same mechanism
and never reach OpenAI, since dragoman is the only thing the client talks to.

**The pocket is filled during the item and attached at `response.output_item.done`**, not at
`.added`. Gemini streams its signature as a delta and DeepSeek's reasoning arrives
progressively, so a client reading the item at `.added` would get an empty pocket and break
on the following turn.

The client must echo it. An application that reconstructs conversation history from rendered
text will drop it and see a Gemini 400. That is a requirement on the client, not an
assumption about it.

### Provider values pass through; dialect names panic

R-12's exhaustive-switch-panics rule applies to values dragoman owns, and only those. An
unknown `--protocol` or `--backend-protocol` panics. An unknown model, effort level, or stop
reason passes through untouched — DeepSeek emits `finish_reason:
insufficient_system_resource`, which OpenAI does not define and which must not crash a
decoder.

### Verification

Field-level claims in this document were verified against official vendor documentation on
2026-07-26. These APIs move; re-verify before treating any of them as settled.

One known gap: Gemini's breaking-changes note says `interaction.status_update` is replaced
by `interaction.in_progress` and `interaction.requires_action`, but only the former appears
in the reference and in the streaming examples. The decoder accepts all three.

## Out of scope

- **The HTTP transport (`serve`).** An inbound listener on `/v1/responses`, so an application
  reaches dragoman by changing a base URL rather than by shelling out. Next, not now: it is a
  second surface over the core above plus SSE framing, which is why the core is written to
  not know what invoked it. One question must be settled before it ships and cannot be
  settled by the code — a client sends its own `Authorization` header, which is useless for
  the backend it gets routed to. Either it is ignored, making the binary localhost-only, or
  it authenticates the caller to dragoman. That decides whether `serve` can be exposed at
  all, and it is a product decision.
- **Additional external dialects.** `anthropic-messages` is the likely first, since it is
  what Claude Code and anything on the Anthropic SDK speaks. It costs three functions —
  decode-request, encode-response, encode-stream — and the other three already exist from
  the backend side.
- **Dialect detection from the body.** There is one input shape; nothing to detect.
- **Legacy Gemini `generateContent`.** Only the current API is targeted.
- **Sampling and decoding parameters** — `temperature`, `top_p`, `top_k`, `seed`,
  `frequency_penalty`, `presence_penalty`, `logit_bias`. Current reasoning models reject
  them, ignore them, or default sensibly. `logit_bias` is additionally meaningless
  cross-provider, since token IDs are tokenizer-specific.
- **`logprobs`, `top_logprobs`, `n`.** Clients that depend on token probabilities are pinned
  to one model by construction and would never route cross-provider.
- **Prompt caching.** Anthropic `cache_control` breakpoints and Gemini cached content are
  neither mapped nor preserved.
- **Subprocess backends.** Routing through a locally installed agent CLI is where this gets
  interesting, but such a harness resolves tool calls itself rather than returning them, and
  making it surface an unresolved call needs an interception mechanism that is not yet
  designed. The executor stays an interface with one HTTP implementation so adding it later
  is additive.
- **Token counting, usage normalisation, and pricing.** The dialects disagree on both the
  names and the meaning of usage fields. Whatever the backend reports is mapped if it has an
  obvious home and dropped if it does not; nothing is computed, converted, or costed.
- **Conversation state.** No session store, no `previous_response_id` resolution, no resume.
  History arrives in the body or it does not exist.
- **Executing tools.**
- **A config file for named backends.** Presets plus flags are enough until the preset table
  is not.
- **Retries, fallback models, rate-limit handling, request logging.**
- **Embeddings, images, audio, batch, files.** Text and tool calls only.

Everything above that a request can still carry is dropped and reported, never silently
discarded — which is what keeps each of them cheap to add later.

## Constraints

- Go. One binary. Bound by `CONSTITUTION.md`.
- **No provider SDKs.** Not `openai-go`, not `anthropic-sdk-go`, not
  `google.golang.org/genai`. Each would impose its own types on the core, which is precisely
  the base format's job, and taking all three would mean maintaining all three. Non-provider
  libraries are fine — flag parsing is `github.com/alecthomas/kong`. `encoding/json` and
  `net/http` are expected to do the rest.
- **kong does not own the exit codes.** Its `FatalIfErrorf` exits `1` unless the error
  implements `ExitCoder`, and `1` is reserved here for "the backend said no". Bad flags must
  surface as `2`, set once at the `cmd/` boundary via `kong.Exit` or an `ExitCoder` error.
- Dependencies flow one way (R-7): the base imports nothing; dialect packages import the
  base only, never each other; transports import the core; `cmd/` wires them together.
- Translation is pure (R-4). Decoding, encoding, and stream transduction are functions from
  data to data with no IO. IO lives in the executor and the transports.
- Unknown fields survive. Decoding into a struct discards what it does not model, so
  anything intended to round-trip is held as raw JSON beside the typed fields rather than in
  a `map[string]any` (R-13). The dropped-field diff depends on this.
- **dragoman does not write prompts.** No text is injected into a system prompt or a message
  to emulate a capability a backend lacks, however well documented the workaround is. Its
  words would compete with the caller's, inflate their input invisibly, and there is no
  principled place to stop once structure can be emulated with prose. A capability the
  backend does not have is a dropped field, reported.
- Secrets never appear in argv. Credentials are therefore read from the environment
  directly, never through kong's `env:""` tag — that tag declares a flag which merely
  *defaults* from the environment, leaving the key settable in argv and listed in `--help`.
- Streaming and buffered translation share one implementation of the mapping.

## Done criteria

Keys for all four providers are in `.env.local` at the repository root, which is gitignored.
Mid-tier models throughout: `gpt-5-mini`, `claude-sonnet-5`, `gemini-3.6-flash`,
`deepseek-v4-flash`.

### Offline — no key, no network

These are the bulk of the suite and run on fixtures.

| check | expected |
|---|---|
| a Responses request round-tripped through the base | byte-identical |
| `--no-execute` for all four backends | a request the target provider would accept |
| fold(translated stream) for every recorded fixture | equals the translated buffered response |
| a body whose level the target dialect spells differently | field relocated, value untouched |
| a body with no level | field absent in the output, not defaulted |
| a body with no output cap, against `anthropic-messages` | the backend fallback is applied |
| the same body, against the other three backends | no cap sent, nothing defaulted |
| `--max-tokens` set, against every backend | the flag value wins over body and fallback alike |
| `--strict` with a droppable field | exit `2`, names the field with its path |
| a `finish_reason` the dialect does not define | passes through, no panic |
| a stream that ends with items open | flushed, no partial item emitted |
| a request with `stream: true`, default flags | events on stdout, one JSON object per line |
| the same request with `--no-stream` | a single buffered body, no events |
| an unknown `--backend-protocol` value | exit `2` |
| any bad flag | exit `2`, never kong's default `1` |

### Live — the case this feature exists for

One request in `openai-responses` form carrying a `get_weather` tool and a prompt that
requires a step of reasoning before the call can be made: the weather in the city that
hosted the 2016 Summer Olympics, so the model must resolve Rio before it can fill in the
arguments.

That same request, unmodified, routed to each backend in turn:

| backend | `--backend-protocol` |
|---|---|
| Gemini | `gemini-interactions` |
| Anthropic | `anthropic-messages` |
| OpenAI | `openai-responses` |
| DeepSeek | `openai-completions` |

Each must return a well-formed `openai-responses` body containing a function call to
`get_weather` with `location` resolved to Rio de Janeiro and `unit` set to `celsius`. The
OpenAI run, where backend and base are the same, must be a faithful pass-through.

Then the return leg, which a single call never exercises: **four independent two-turn
conversations, one per backend.** Each backend's own turn-one response is fed back to that
same backend as turn two, carrying the tool result. This proves call identity survives
translation in both directions, and that provider state survived a turn boundary in the
client's hands. DeepSeek is the sharp case, and the reason it is in the matrix: it returns
400 rather than degrading quietly if the state it handed out does not come back, so it
catches an unfaithful round-trip that the others would absorb in silence.

Finally the same request with `stream: true` against each backend, where the folded event
stream equals the buffered response already captured.

## Open questions

- **The riskiest assumption:** that a Responses base holds all four dialects without a
  per-dialect escape hatch that quietly becomes the real API. Provider state is a typed
  field rather than a bag, which covers the known case. A second such field appearing would
  mean the base is not a base.
