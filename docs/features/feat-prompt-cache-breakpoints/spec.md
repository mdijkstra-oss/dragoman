---
slug: prompt-cache-breakpoints
type: feat
status: draft
created: 2026-07-30
supersedes: feat-protocol-translating-llm-proxy#out-of-scope/prompt-caching
---

------

# Feature: Prompt cache breakpoints

## Purpose

Caching was out of scope because a breakpoint is a marker on a content block and the base had
nowhere to put one. OpenAI's newest models added it to the base:

```json
{ "type": "input_text", "text": "…", "prompt_cache_breakpoint": { "mode": "explicit" } }
```

So the caller marks what to cache and dragoman carries that to a backend spelling it differently.

## What the providers do

Verified live 2026-07-30. The 5.6 family is `gpt-5.6-luna`, `-sol`, `-terra` — there is no plain
`gpt-5.6`.

| provider | marker | TTL | count |
|---|---|---|---|
| OpenAI, 5.6 family | `prompt_cache_breakpoint: {"mode":"explicit"}` on `input_text`, `input_image`, `input_file` (Responses); `text`, `image_url`, `input_audio`, `file`, `refusal` (Chat Completions) | request-wide `prompt_cache_options.ttl`; `30m` only | no cap found — 4, 5, 8, 20 all accepted |
| OpenAI, pre-5.6 | `400` naming whichever field was sent; the breakpoint alone is refused, not just `prompt_cache_options` | — | — |
| Anthropic | `cache_control: {"type":"ephemeral","ttl":"5m"\|"1h"}` on a content block | per breakpoint; `5m` or `1h` only | max 4, enforced |
| Gemini Interactions | none | — | — |
| DeepSeek | accepts and **silently ignores** both fields; `200`, no error, no effect | — | — |

Both providers validate the values they own, and both name the offending parameter. Anthropic's
refusals are path-shaped, which is what makes forwarding a `30m` usable rather than merely
honest:

```
messages.0.content.0.text.cache_control.ephemeral.ttl: Input should be '5m' or '1h'
A maximum of 4 blocks with cache_control may be provided. Found 5.
```

Usage lands in `usage.input_tokens_details` on Responses and `usage.prompt_tokens_details` on
Chat Completions, carrying `cache_write_tokens` beside `cached_tokens`, buffered and streamed
alike. Anthropic reports `cache_creation_input_tokens` and `cache_read_input_tokens`, and splits
writes by TTL bucket under `cache_creation.ephemeral_5m_input_tokens` / `_1h_`. The base reads the
flat total and not the buckets — one field, one meaning, and no arithmetic.

## Scope

The base is `openai-responses`, so the field is OpenAI's, under OpenAI's name.
`prompt_cache_key` and `prompt_cache_options` are top-level and carried as such. A decoder reads
whether a breakpoint is present, never what `mode`, a key, or a TTL means.

| dialect | breakpoint | `prompt_cache_key` | `prompt_cache_options` |
|---|---|---|---|
| `openai-responses` | verbatim | verbatim | verbatim |
| `openai-chat-completions` | verbatim | verbatim | verbatim |
| `openai-completions` | verbatim | verbatim | verbatim |
| `anthropic-messages` | `cache_control` on the same block | dropped, no field | `.ttl` placed verbatim; `.mode` dropped, no field |
| `gemini-interactions` | dropped, `unsupported` | dropped | dropped |

Three dialects need no mapping code — same field name both sides. The marker is not a `variant`
field, and adding one is a sign the change is misplaced. `claude-cli` needs nothing: it speaks
`anthropic-messages` and its executor keeps content as `json.RawMessage`.

**Refusals are forwarded, not guessed at.** The marker goes to every dialect with a field for it
with no model check, and `30m` goes into `cache_control.ttl` which takes only `5m` and `1h`. A
`400` naming the field is a diagnosis; a silent strip is `200` at full price forever. Support is
gated on the model, so it could not live in the service table anyway. Anthropic's four-breakpoint
limit is Anthropic's to enforce — a fifth is forwarded, not trimmed.

`no_schema` runs the other way, and the difference is that its workaround would be writing a
prompt. Nothing here needs one.

**Where that rule buys nothing: DeepSeek.** It returns `200` and ignores both fields, so a caller
marking blocks against it gets no caching and no signal, and dropping would have produced the same
silence. Forwarding is still what happens — dragoman has no way to tell an endpoint that ignores
a field from one that honours it, and guessing per endpoint is the table this feature refuses to
build. Named here so it is a known limit rather than a bug report.

**Absent stays absent.** No `prompt_cache_options` means `cache_control` with `type` alone and
Anthropic's own default. Expect this to read as a bug: spelling out `"ttl": "30m"` is refused
where omitting it succeeds, though `30m` is OpenAI's default. OpenAI's TTL is request-wide and
Anthropic's is per breakpoint, so one value fans out onto every `cache_control`.

**dragoman never adds a breakpoint** — not on a long prompt, not on tools, not on the system
message. It is a spending decision. The cost: `openai-responses` allows breakpoints only on input
content blocks, so tool definitions cannot be marked, which in an agent loop is the largest stable
prefix there is. Anthropic can express it, the base cannot, so it does not travel.

**Cache writes are the instrument.** `InputDetails` reports `cached_tokens` and nothing about
writes, so OpenAI's `cache_write_tokens` and Anthropic's `cache_creation_input_tokens` are read
off the wire and discarded — quietly, since response-side drops never diff `usage`. Without a
write count, `cached_tokens: 0` on a first request means either the marker did nothing or the
cache was just written. `InputDetails` gains one field, mapped through `CountUsage` in all four
dialects, costed nowhere.

## Out of scope

- **A model capability table**, shipped, remote, or inferred from a model name.
- **TTL conversion**, including a flag choosing which Anthropic TTL a `30m` request becomes. That
  flag selects a price.
- **Automatic or heuristic breakpoint placement.**
- **Gemini caching.** No inline field, and both routes are stateful — `CachedContent` on the
  legacy `generateContent` API, or `previous_interaction_id`. A stateless proxy reaches neither,
  so the drop is permanent rather than pending.
- **Cost reporting**, and **cache-aware request shaping** — reordering for a stable prefix,
  hoisting instructions, normalising whitespace.

## Constraints

- Bound by `CONSTITUTION.md` and the parent spec. No new dependency.
- No new field on `base.Backend` or on `variant`. No dialect branches on a backend name.
- **A request with no marker is byte-identical on the wire to one built before this feature.**

## Done criteria

### Offline

| check | expected |
|---|---|
| breakpoint → each of the three OpenAI dialects | survives byte-identical; same marker bytes across all three |
| breakpoint → `anthropic-messages` | `cache_control: {"type":"ephemeral"}` on the same block |
| `ttl: "30m"` → Anthropic | written verbatim, nothing converted |
| no `prompt_cache_options` → Anthropic | `type` alone, no `ttl` key |
| `mode` → Anthropic | dropped; exit `2` under `--strict`, naming the path |
| breakpoint → `gemini-interactions` | dropped, `unsupported` |
| five breakpoints → Anthropic | five blocks, nothing trimmed |
| an unrecognised `mode` string | forwarded where the field exists, no panic |
| **no breakpoint, every dialect** | bytes identical to the pre-feature fixture |
| recorded responses carrying `cache_creation_input_tokens` / `cache_write_tokens` | surfaced as the write count |
| fold(translated stream) | equals the translated buffered response |

### Live

Skipped, not failed, without keys. One request with a breakpoint, sent twice: run one reports a
write count above zero and `cached_tokens: 0`, run two reports `cached_tokens` above zero. The
pair is the proof, neither run alone.

This is already observed against `gpt-5.6-luna` on both OpenAI paths, so the criterion is a
regression test rather than a hypothesis — run one `{"cache_write_tokens": 4190, "cached_tokens":
0}`, run two `{"cache_write_tokens": 0, "cached_tokens": 4190}`, identical whether buffered or
streamed.

Two things the fixture and matrix must get right or the suite passes while testing nothing:

- `testdata/cached.json` needs **more than 1,024 tokens before the breakpoint**. Anthropic's
  minimum is 512 on Opus 5 and 1,024 on Sonnet 5, and a short prompt is not an error — it returns
  zeroes from both runs and reads like a broken marker.
- The **control** is the same fixture with the marker removed, run twice. OpenAI and Gemini cache
  implicitly unasked, so a hit on run two proves nothing until diffed against it.

The refusals, all observed, all exit `1` carrying the provider's own words:

| request | message |
|---|---|
| breakpoint → `--model gpt-5.1` | `prompt_cache_breakpoint is not supported on this model` |
| `ttl: "30m"` → Anthropic | `messages.0.content.0.text.cache_control.ephemeral.ttl: Input should be '5m' or '1h'` |
| five breakpoints → Anthropic | `A maximum of 4 blocks with cache_control may be provided. Found 5.` |

Gemini exits `0` with the drop on stderr; DeepSeek exits `0` having ignored it.

## Open questions

- **Should an unread `usage` field become a drop?** Never diffing `usage` is how the write count
  went missing unsignalled; extending it would also surface every cost field and service tier.
- **`openai-chat-completions` still has no recorded fixture**, though its usage reporting is now
  verified live rather than reasoned from documentation.
- **Anthropic's placements beyond a message content block are untested.** `system`, `tools`,
  `tool_use` and `tool_result` are documented targets, and only a message block was exercised. It
  matters for the count limit: a caller's four input breakpoints are the same four slots a future
  tools breakpoint would need.
