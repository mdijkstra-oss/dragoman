# todo

## Prompt cache breakpoints

Specced: `docs/features/feat-prompt-cache-breakpoints/spec.md`, which supersedes the parent
spec's out-of-scope entry for prompt caching. Provider behaviour in it is verified live against
OpenAI, Anthropic and DeepSeek rather than read from documentation.

The variant question that started this is closed: `cache_write_tokens` arrives under
`prompt_tokens_details` on `openai-chat-completions` whether the run is buffered or streamed, so
the marker is invariant across both `/chat/completions` formats on the response side as well as
the request side. What remains open is in that spec.
