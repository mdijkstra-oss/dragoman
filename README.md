# dragoman

A stateless protocol-translating LLM proxy.

A request goes in written in `openai-responses`, gets executed against a provider
speaking another dialect, and comes back as `openai-responses`. The client never
learns which provider answered.

```sh
dragoman --backend anthropic --model claude-sonnet-5 < request.json
dragoman --backend gemini --model gemini-3.6-flash --stream < request.json
```

One request in, one response out. No session, no stored conversation, no agent
loop, no tools executed on the caller's behalf. Everything needed to answer is
in the body that arrived, which is what makes the binary interchangeable with
the endpoint it stands in front of.

dragoman moves structure and never meaning. Values pass through untranslated,
prompts are never written, capabilities are never emulated.

## Dialects

The application speaks `openai-responses` and receives `openai-responses` back.
Four dialects are available as a backend:

| value | wire |
|---|---|
| `openai-responses` | `POST /v1/responses` |
| `openai-completions` | `POST /v1/chat/completions` — also DeepSeek, OpenRouter, and every other compatible endpoint |
| `anthropic-messages` | `POST /v1/messages` |
| `gemini-interactions` | `POST /v1beta/interactions` |

A dialect is a wire shape, not a vendor. `openai-completions` names a body
format several companies serve.

## Backends

A backend is a dialect at a URL with a key in the environment:

```sh
dragoman --backend-protocol gemini-interactions \
         --endpoint https://generativelanguage.googleapis.com/v1beta \
         --model gemini-3.6-flash < req.json
```

`--backend` is sugar that fills those from a preset table, which also carries
the backend's quirks record. Presets are a convenience and stay one: anything a
preset does is expressible in flags, so an endpoint nobody anticipated is a
command line rather than a patch.

| preset | dialect | key |
|---|---|---|
| `openai` | `openai-responses` | `OPENAI_API_KEY` |
| `openai-chat` | `openai-completions` | `OPENAI_API_KEY` |
| `anthropic` | `anthropic-messages` | `ANTHROPIC_API_KEY` |
| `gemini` | `gemini-interactions` | `GEMINI_API_KEY` |
| `deepseek` | `openai-completions` | `DEEPSEEK_API_KEY` |
| `openrouter` | `openai-completions` | `OPENROUTER_API_KEY` |

Credentials come from the environment only, never from a flag. argv is visible
in `ps`, in shell history, and in CI logs.

## Model and effort

The **field** is structural, and dragoman maps it — each dialect keeps the model
and the reasoning level somewhere different:

| dialect | reasoning level lives at |
|---|---|
| `openai-responses` | `reasoning.effort` |
| `openai-completions` | `reasoning_effort` |
| `anthropic-messages` | `output_config.effort` |
| `gemini-interactions` | `generation_config.thinking_level` |

The **value** is opaque. It passes through byte for byte: never normalised,
case-folded, validated against a set, or mapped onto a nearer neighbour.
Reasoning levels do not denote comparable strength across providers, so any
mapping dragoman invented would be a guess wearing the costume of a conversion.

> dragoman owns the set of dialects. Providers own the set of models and levels.

Precedence is the `--model`/`--effort`/`--max-tokens` flag, then the value in the
body, then nothing. **Absent stays absent** — filling in a level nobody asked for
silently overrides a provider default and changes both cost and behaviour.

`anthropic-messages` is the only backend that requires an output cap. When
neither flag nor body supplies one, its quirks record contributes a fallback of
20000; no other backend ever receives an invented value.

## Streaming

`stream` is an **output** setting. It decides what the caller receives, not how
dragoman talks to the backend.

- default — one `openai-responses` body on stdout, written once, when the answer
  is complete.
- `stream: true` in the body, or `--stream` — the `openai-responses` event
  sequence, one event per line, written and flushed per event.

What is emitted is the complete Responses lifecycle, not the deltas alone, so a
stock SDK reconstructs the answer from that sequence the way it always has.
Framing is newline-delimited JSON; re-framing the identical sequence as SSE is
what an HTTP transport would do.

The two axes are independent, and one invariant keeps them honest:

> **Folding a translated event stream equals the translated buffered response**,
> for every fixture and every backend.

## Provider state

Every provider returns something it wants handed back unaltered, and each binds
it to the item it arrived on. dragoman carries it in the item's
`encrypted_content` and puts it back where it belongs — a signature on an
Anthropic thinking block, a Gemini thought signature (which may ride on the call
itself), DeepSeek's reasoning content, OpenAI's encrypted reasoning.

**A conversation lives on one backend for its whole life.** Histories are not
interchangeable between providers and never could be.

Because dragoman is stateless, that state survives a turn boundary only in the
client's hands: echo the response's output items back in the next request's
input. An application that reconstructs history from rendered text will drop it.

## Dropped fields

A drop is any key present in a body that no typed field consumed — computed,
never hand-maintained. Entries carry a path, not a name
(`input[3].content[1].signature`), because skip-and-continue makes this the
caller's only signal.

- **unsupported** — the target dialect has no equivalent and never will.
- **unimplemented** — dragoman could map it and does not yet.

By default the response is returned and drops go to stderr. `--strict` makes any
drop an error: exit `2`, nothing sent. Silence is not an option — a dropped field
produces a confident, well-formed, wrong answer.

## Diagnostics

| flag | effect |
|---|---|
| `--no-execute` | translate the request, print what would have been sent, make no call |
| `--raw` | return the backend's untranslated response |
| `-v` | the translation and its dropped fields to stderr |

`--raw` returns exactly what the backend sent. For a backend whose quirks record
streams it upstream regardless of the output mode, that is the event transcript
rather than a body — which is the point: it is what a mapper bug would have been
read from.

## Exit codes

| code | meaning |
|---|---|
| `0` | a response was returned and translated |
| `1` | the backend said no: HTTP 4xx/5xx, or a provider error body |
| `2` | dragoman could not run: unparseable input, unknown dialect, missing key, bad flags, a required field with no source, or a drop under `--strict` |

`2` is reserved for "the tool could not run", which is the only thing that makes
it trustworthy in a hook or a CI step.

## Layout

Dependencies flow one way. The base imports nothing; dialect packages import the
base only, never each other; transports import the core; `cmd/` wires them
together.

```
internal/base/                 the openai-responses format — types, the event
                               grammar, the emitter, the fold, the drop diff
internal/dialect/              the shape every wire format implements
internal/dialect/*/            one package per dialect
internal/core/                 flag precedence, backend selection, the pipeline
internal/catalog/              dialect table and backend presets
internal/transport/pipe/       stdin to stdout
cmd/dragoman/                  argv, streams, exit codes — and nothing else
```

The core takes a request and returns a response or an event sequence, and never
touches `os.Stdout`. An HTTP listener is a second surface over the same core.

## Building and testing

```sh
go build -o bin/dragoman ./cmd/dragoman
go test ./...
```

The suite runs offline against responses recorded from all four live backends in
`testdata/fixtures/`. No key and no network are needed.
