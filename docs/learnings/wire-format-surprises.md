# Wire-format surprises: where live behaviour contradicted the spec and the docs

Probed live against all four providers on 2026-07-26 with real keys. Every entry
below is a captured HTTP response, not training data or documentation. Where the
vendor docs and the wire disagree, the wire wins.

The spec was written from documentation. Six of its factual claims turned out to
be wrong on the wire, and two of them changed the design.

## Claims that were wrong, and what replaced them

### 1. DeepSeek does not 400 when reasoning content is not echoed back

The spec named this as the sharp case in the live matrix: *"it returns 400 rather
than degrading quietly if the state it handed out does not come back, so it
catches an unfaithful round-trip that the others would absorb in silence."*

Six variants were tested against `deepseek-v4-flash` — `reasoning_content`
present, absent, `""`, `null`, mutated, and index-stripped — plus a three-turn
two-tool-round conversation with it stripped at every turn. **All returned 200.**
Only the token accounting shifts (477 vs 453 prompt tokens).

**The sharp case is Anthropic and Gemini instead**, and both were caught by the
two-turn matrix during development:

- Anthropic: mutating a thinking block's `signature` → 400; omitting the
  `signature` field → 400; *omitting the whole thinking block* → 200. So the
  signature must round-trip byte-for-byte, but its presence is optional.
- Gemini: omitting a replayed call's `signature` → 400 `Request contains an
  invalid argument.`; tampering → 400 `Corrupted thought signature.`

dragoman still round-trips DeepSeek's `reasoning_content` faithfully. The change
is that it is no longer the backend that proves the mechanism works.

### 2. DeepSeek's reasoning effort is not restricted to `high|max`

The server's own deserializer enumerates five values and all five return 200:

```
{"reasoning_effort":"bogus"}
→ 400 unknown variant `bogus`, expected one of `high`, `low`, `medium`, `max`, `xhigh`
```

`none` and `minimal` are the only rejects. No restriction was encoded in the
quirks record — which is the correct outcome anyway, since providers own the set
of levels and dragoman passes values through.

### 3. DeepSeek reads the effort at the top level, not inside `thinking`

DeepSeek's own docs describe `thinking.reasoning_effort`. Live, that path is
**silently discarded** — DeepSeek ignores unknown keys at every nesting level, so
`{"thinking":{"reasoning_effort":"bogus"}}` returns 200 with the setting having
no effect. Only top-level `reasoning_effort` is validated and read.

This is the exact failure mode the spec exists to prevent: a mis-*placed* key is
silent where a mis-*encoded* value is loud.

### 4. `/v1` on DeepSeek's base URL is a convention, not a constraint

The spec says DeepSeek "has no `/v1` in its base URL". Both
`https://api.deepseek.com/chat/completions` and
`https://api.deepseek.com/v1/chat/completions` return 200 — `/v1` is an accepted
alias, not a redirect. The preset uses the shorter form to match DeepSeek's docs,
but nothing breaks if a user pastes the longer one into `--endpoint`.

### 5. The output-cap field name is not portable inside the `openai-*` family

This one changed the design. Same dialect, two spellings, each rejecting or
ignoring the other:

| endpoint | reads | does with the other |
|---|---|---|
| OpenAI `/v1/chat/completions` | `max_completion_tokens` | 400 on `max_tokens` |
| DeepSeek `/chat/completions` | `max_tokens` | silently ignores `max_completion_tokens` |

So the cap's field name is a **quirks-record entry** (`MaxTokensField`), not a
dialect constant. It is a second instance of the spec's "a field's home is
occasionally model-dependent" — here it is endpoint-dependent within one dialect.

### 6. Gemini streams a call's signature inline, not as a delta

The spec says Gemini "streams its signature as a delta". That is true for
`thought` steps and **false for `function_call` steps**, where the signature
arrives in full on `step.start`. When the model goes straight to a call with no
preceding thought, the state is bound to the call itself.

This changed the base: provider state is a field on *an item*, not on a reasoning
item. The emitter carries it on whichever item it arrived on, and the Gemini
encoder puts it back there.

## Two design corrections the live matrix forced

Both were found by running four two-turn conversations, and neither would have
been caught by a one-shot call.

### Minted item ids break OpenAI's encrypted reasoning

The emitter mints `item_id` because the source dialects do not carry one. OpenAI
verifies its encrypted reasoning **against the id it issued**:

```
400 The encrypted content for item rs_0 could not be verified.
    Reason: Encrypted content item_id did not match the target item id.
```

An id is now minted only when the source dialect carries none. `openai-responses`
supplies its own and it is kept verbatim; the other three have block indices or
step ordinals rather than item identity, so they still get minted ids.

### Anthropic requires the `thinking` field even when it is empty

`claude-sonnet-5` defaults `thinking.display` to `omitted`, so a thinking block
comes back with a real signature and an empty `thinking` string. Re-sending it
with `omitempty` applied dropped the field:

```
400 messages.1.content.0.thinking.thinking: Field required
```

Block serialisation is now per-type, so a thinking block always carries its text
and a text block never carries a signature.

## Things worth knowing that were not wrong, just unstated

- **Anthropic rejects unknown top-level fields** (`Extra inputs are not
  permitted`) rather than ignoring them, so its encoder builds a body from
  modelled fields only. Gemini does the same at every depth. DeepSeek ignores
  unknown keys everywhere. Only `openai-responses` can pass a body through.
- **Every Anthropic `tool_use` block carries an undocumented
  `"caller":{"type":"direct"}`.** Modelling it keeps it out of the drop diff on
  every single tool call; it is optional on the way back.
- **Gemini's Interactions API rejects camelCase** (`generationConfig` → 400 "Did
  you mean 'generation_config'?"), unlike the legacy `generateContent` endpoint.
  Do not share structs between them.
- **Gemini has two incompatible error envelopes** on the same endpoint: an object
  with a string `code` from the Interactions layer, and a top-level JSON **array**
  with an integer `code` from the API gateway. A bad API key returns 400, not 401,
  through the array form — the most likely path to hit in the field.
- **OpenAI re-encrypts its reasoning pocket between frames.** The
  `encrypted_content` on `response.output_item.done` differs from the one on
  `response.completed` for the same call. The fold-invariant test compares that
  field by presence rather than by bytes.
- **`gpt-5.4-mini` on `/v1/chat/completions` rejects `reasoning_effort` together
  with function tools** for any value except `none`. dragoman passes it through
  and lets the provider refuse, per "nothing infers that a model name is wrong for
  a backend".
- **`gpt-5-mini` and `gpt-5.4-mini` are not interchangeable at the edges**: the
  field's home is the same, but `minimal` works only on the older one and
  `none`/`xhigh` only on the newer. Another argument for passing values through.

## The portable intersection

`low`, `medium`, `high` are accepted by all four recommended models. Everything
else is provider-specific: `none` (OpenAI only), `minimal` (Gemini and
`gpt-5-mini`), `xhigh` (all but Gemini), `max` (Anthropic and DeepSeek only).

This is not a mapping table — dragoman passes values through byte for byte. It is
the safe value set for a test matrix where one unmodified request goes to every
backend.

## Usage: one output shape means one counting method

The four providers report token usage in three different *meanings*, not just
three different spellings, and the difference is invisible because they all use
a field called "output tokens".

| provider | reasoning tokens are | field |
|---|---|---|
| `openai-responses` | **inside** `output_tokens` | `output_tokens_details.reasoning_tokens` |
| `anthropic-messages` | **inside** `output_tokens` | `output_tokens_details.thinking_tokens` |
| `openai-completions` | **inside** `completion_tokens` | `completion_tokens_details.reasoning_tokens` |
| `gemini-interactions` | **beside** `total_output_tokens` | `total_thought_tokens` |

Captured live for one identical request, before this was mapped:

```
gemini     total_output_tokens 25    total_thought_tokens 90     ← siblings
anthropic  output_tokens 158         thinking_tokens 34          ← subset
deepseek   completion_tokens 112     reasoning_tokens 25         ← subset
```

Forwarding those numbers unchanged makes `usage.output_tokens` mean "the answer"
on one backend and "the answer plus the reasoning" on three others — a caller
comparing models or estimating cost reads 25 against 160 and concludes a 6×
difference that does not exist. The real figures are 115 and 158.

This is not a value dragoman should pass through. The base **is**
`openai-responses`, and Responses defines the arithmetic: `output_tokens`
includes reasoning, `output_tokens_details.reasoning_tokens` names the subset,
and `total_tokens` is the two sides added. Assembling Gemini's two siblings into
that shape is the same structural mapping as relocating the reasoning level —
the meaning is the base's, not one dragoman invented.

`TestUsageMeansTheSameThingEverywhere` asserts the arithmetic holds for every
backend on both paths: `input + output == total`, and `reasoning <= output`.

Two figures that are real and remain after the fix, for the same request:

- **Input differs 5×** — 104 (Gemini) vs 329 (DeepSeek) vs 516 (Anthropic).
  Almost none of that is the tokenizer: the bare prompt costs 22 / 23 / 37. It
  is the tool-use scaffolding each provider adds server-side, which costs 82 /
  306 / 479 for the same one-function schema. dragoman sends the same tool to
  all three; `--no-execute` shows the bodies.
- **DeepSeek served 256 of its 329 input tokens from cache** on that run, so its
  input is not priced the way the raw count suggests either. That is now
  reported as `input_tokens_details.cached_tokens`.
