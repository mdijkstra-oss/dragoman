# AGENTS.md

This file is the working guide for coding agents in this repository. Keep changes small, preserve unrelated work, and update this guide when the project workflow materially changes.

## Project overview

Dragoman is a stateless Go proxy and CLI that translates OpenAI Responses requests into provider-specific wire protocols, then translates provider responses and streams back into OpenAI Responses form.

- `cmd/dragoman`: main CLI and HTTP server entry point.
- `cmd/claude-bridge`: development-only Anthropic Messages bridge to the Claude CLI.
- `internal/base`: protocol-neutral request, response, event, piece, and drop types.
- `internal/dialect`: provider protocol encoders and decoders.
- `internal/core`: backend selection, translation, execution, streaming, and folding.
- `internal/catalog`: built-in dialect and backend registration.
- `internal/config`: embedded/default service configuration and YAML overrides.
- `internal/transport`: stdin/stdout and HTTP adapters.
- `testdata/fixtures`: representative buffered and streaming provider payloads.

The module targets Go 1.25.1. Production dependencies are intentionally few.

## Development workflow

Run these from the repository root:

```sh
go test ./...
go test ./internal/core
go test ./internal/core -run TestName
go fmt ./...
go vet ./...
go build ./cmd/dragoman ./cmd/claude-bridge
```

Useful manual targets are documented in the README and Makefile:

```sh
make run PROMPT="Say hello."
make serve
make serve-dev
make bridge
```

Provider-backed commands may spend money and require credentials. Prefer unit tests, fixtures, `--no-execute`, or `TRANSLATE_ONLY=1` unless a live call is explicitly needed.

## Change guidelines

- Read the relevant request path end to end before editing: parse/resolve in `internal/core`, encode/decode in the selected dialect, and emit/fold in `internal/base`.
- Keep the internal representation provider-neutral. Provider-specific field names and quirks belong in `internal/dialect/<protocol>`.
- Treat streaming and buffered behavior as two views of the same translation. Changes to one usually require coverage for the other.
- Preserve loss reporting. Unsupported non-empty fields must be represented as `base.Drop` values; `--strict` depends on accurate drops.
- Do not silently move credentials into flags, config values, logs, fixtures, or command lines. Configuration stores environment-variable names, never secrets.
- The default service table is embedded. Changes to `internal/config/dragoman.yaml` are source changes and require rebuilding.
- Keep the server stateless and avoid provider calls in translate-only paths.
- Follow existing Go style: straightforward control flow, explicit errors, short focused helpers, and comments that explain constraints or intent rather than restating code.
- Use `gofmt` on every changed Go file. Avoid unrelated formatting or refactors.

## Tests and fixtures

- Add or update focused tests alongside the package being changed.
- For dialect changes, cover request encoding and response decoding. Include both buffered bodies and stream frames when the provider supports both.
- Put stable provider examples in `testdata/fixtures/<provider>` and keep credentials, personal data, and unstable generated values out of fixtures.
- Assert semantic output, event ordering, usage accounting, and dropped-field paths where relevant.
- Bug fixes should include a regression test that fails before the fix.
- Run `go test ./...` before handing off. Also run `go vet ./...` and builds when changes affect commands, configuration, transports, or concurrency.

## Documentation and compatibility

- Update `README.md` when changing user-visible flags, environment variables, routes, service configuration, defaults, or examples.
- Preserve the public OpenAI Responses-facing behavior unless the task explicitly changes it.
- Keep exit-code semantics intact: `0` for an answer, `1` for backend refusal, and `2` for an unsendable request.
- Call out unavoidable lossy translation explicitly in code, tests, and user-facing documentation.

## Before finishing

1. Inspect `git diff` and ensure only intended files changed.
2. Format changed Go code.
3. Run the smallest relevant test during development, then `go test ./...`.
4. Report validation performed and any tests not run, including why.
