# make run PROMPT="..." [MODEL=] [REASONING=] [ARGS=]
# make run PROMPT=@prompt.txt      the prompt is read from that file
# make run < request.json          no PROMPT: a whole request off stdin
# make serve [ADDR=]               the built-in table over HTTP
# make serve-dev [ADDR=]           the same, rebuilt and restarted on a change
# make bridge [BRIDGE_ADDR=]       anthropic-messages answered by the claude CLI
# Keys come from .env.local. ARGS goes straight to dragoman (--stream, --raw,
# --no-execute, -v ...). JQ=. prints the whole response.

MODEL ?= gemini-3.6-flash
BACKEND ?= $(strip $(shell case '$(MODEL)' in \
             (gemini-*) echo gemini ;; (claude-*) echo anthropic ;; \
             (deepseek-*) echo deepseek ;; (gpt-*|o[0-9]*) echo openai ;; \
             (*/*) echo openrouter ;; esac))

JQ ?= {status, model, text: ([.output[] | select(.type=="message") | .content[].text] | join("")), calls: [.output[] | select(.type=="function_call") | {name, arguments: (.arguments | fromjson)}], usage}

ADDR ?= 127.0.0.1:8080

BIN := bin/dragoman
BRIDGE := bin/claude-bridge
BRIDGE_ADDR ?= 127.0.0.1:8082
# The service table is embedded, so a table edit is a source change like any
# other and has to rebuild the binary.
SOURCES := $(shell find cmd internal \( -name '*.go' -o -name '*.yaml' \) 2>/dev/null)
export PROMPT MODEL REASONING

.PHONY: run serve serve-dev bridge

$(BIN): $(SOURCES)
	@go build -o $(BIN) ./cmd/dragoman

# No PROMPT means the request itself is on stdin. Otherwise jq builds the body,
# so quoting in PROMPT cannot break it.
run: $(BIN)
	@set -a; [ -f ./.env.local ] && . ./.env.local; set +a; \
	{ case "$$PROMPT" in "") exec cat ;; @*) PROMPT=$$(cat "$${PROMPT#@}") ;; esac; \
	  jq -n --arg prompt "$$PROMPT" --arg reasoning "$$REASONING" \
	    '{input: [{type:"message", role:"user", content:[{type:"input_text", text:$$prompt}]}]} \
	     + (if $$reasoning == "" then {} else {reasoning:{effort:$$reasoning}} end)'; } \
	| $(BIN) --backend $(BACKEND) --model "$$MODEL" --timing $(ARGS) \
	| jq '$(JQ)'

# No --config: the embedded table is the whole of what this serves. A service
# whose key is missing from .env.local still starts and fails when it is called.
SERVE = set -a; [ -f ./.env.local ] && . ./.env.local; set +a; \
	exec $(BIN) serve --addr $(ADDR) $(ARGS)

serve: $(BIN)
	@$(SERVE)

# serve-dev is serve rebuilt and restarted on every source change. The .yaml is
# there because the service table is embedded: editing it is editing the binary.
# watchexec runs the build itself rather than calling back into make, so that a
# restart is the server dying and nothing else reporting on it.
serve-dev:
	@command -v watchexec >/dev/null || { \
	  echo "serve-dev needs watchexec: brew install watchexec" >&2; exit 1; }
	@exec watchexec --restart --exts go,yaml --watch cmd --watch internal \
	  -- 'go build -o $(BIN) ./cmd/dragoman && { $(SERVE); }'

# No .env.local here: the bridge strips every ANTHROPIC_* variable from the CLI
# it spawns, and loading keys it would only throw away invites confusion.
$(BRIDGE): $(SOURCES)
	@go build -o $(BRIDGE) ./cmd/claude-bridge

bridge: $(BRIDGE)
	@exec $(BRIDGE) --addr $(BRIDGE_ADDR) $(ARGS)
