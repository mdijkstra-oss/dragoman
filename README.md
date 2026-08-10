# dragoman

> **dragoman** (n.) — a professional interpreter and guide, often acting as both linguist and cultural mediator for travelers, merchants, or diplomats.

dragoman translates between LLM protocols. It reads an [openai-responses](https://platform.openai.com/docs/api-reference/responses) request, converts it to the provider's protocol, and converts the answer back. One request format reaches Claude, Gemini and DeepSeek alike.

## Install

```shell
go install github.com/mdijkstra-oss/dragoman/cmd/dragoman@latest
```

## Make a request

```shell
export ANTHROPIC_API_KEY=...
echo '{"input":"Say hello."}' | dragoman --backend anthropic --model claude-sonnet-5
```

| Flag | What it does |
| --- | --- |
| `--backend` | Named service from the table. |
| `--model` | Model to request. Passed through untouched. |
| `--stream` | Deliver the events, one JSON object per line. |
| `--no-execute` | Print the translated request. Make no call. |
| `--raw` | Return the provider's answer untranslated. |
| `--max-tokens` | Output cap. |
| `--effort` | Reasoning level. Passed through untouched. |
| `-f`, `-o` | Read from a file, write to a file, instead of stdin and stdout. |
| `--verbose` | Write the translation and every dropped field to stderr. |
| `--timing` | Write the elapsed time to stderr. |

## Start as server

```shell
dragoman serve --addr 127.0.0.1:8080

# same body as stdin takes, with the service before the first slash of the model
# "stream": true comes back as server-sent events
curl -s http://127.0.0.1:8080/responses -d '{"model":"anthropic/claude-sonnet-5","input":"Say hello."}'
```

| Endpoint | Returns |
| --- | --- |
| `POST /responses` | The LLM's response. |
| `GET /services` | The list of services, and which of them are available. |
| `GET /health` | Whether the server is up. |

## Services

These services are built in:

| Service | Protocol | Key |
| --- | --- | --- |
| `openai` | `openai-responses` | `OPENAI_API_KEY` |
| `anthropic` | `anthropic-messages` | `ANTHROPIC_API_KEY` |
| `gemini` | `gemini-interactions` | `GEMINI_API_KEY` |
| `deepseek` | `openai-completions` | `DEEPSEEK_API_KEY` |
| `openrouter` | `openai-completions` | `OPENROUTER_API_KEY` |
| `openai-chat` | `openai-completions` | `OPENAI_API_KEY` |

`dragoman config` prints the whole table with its endpoints, in the same YAML a config file uses.

## Adding your own

```yaml
mode: append # keeps the built-in services; 'override' serves only this file's
groq:
  endpoint: https://api.groq.com/openai/v1
  protocol: openai-completions
  auth: GROQ_API_KEY
```

Load it with `--config`.

## Lossy translation

Not every field survives every translation. Anything that cannot be carried is dropped. `--verbose` names each one on stderr, and says what the loss cost wherever dragoman has something to say about it. `--strict` turns any drop into an error before the call goes out.

## Container

```shell
docker build -t dragoman .
docker run -p 8080:8080 -e ANTHROPIC_API_KEY dragoman
```

## More

`dragoman --help`, and `dragoman <command> --help`, carry every flag. Licence: [AGPL-3.0](LICENSE).
