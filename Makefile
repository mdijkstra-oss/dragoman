# make run PROMPT="..." [MODEL=] [REASONING=] [ARGS=]
# make run PROMPT=@prompt.txt      the prompt is read from that file
# make run < request.json          no PROMPT: a whole request off stdin
# make run BACKEND=claude-cli      the locally installed CLI; no key needed
# make serve [ADDR=]               the built-in table over HTTP
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
export PROMPT MODEL REASONING

.PHONY: run serve

$(BIN): $(shell find cmd internal -name '*.go' 2>/dev/null)
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
serve: $(BIN)
	@set -a; [ -f ./.env.local ] && . ./.env.local; set +a; \
	exec $(BIN) serve --addr $(ADDR) $(ARGS)
