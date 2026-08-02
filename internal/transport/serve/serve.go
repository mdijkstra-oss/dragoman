// Package serve is the HTTP transport. It owns what a listener adds and
// nothing more: resolving a service out of the model the body names, framing
// the answer as one JSON body or as server-sent events, and mapping a failure
// to a status.
// Flag precedence, backend selection, translation and the dropped-field diff
// are core concerns it hands over and never implements, which is what makes
// this a sibling of the pipe rather than a second pipeline.
package serve

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/matthijn/dragoman/internal/base"
	"github.com/matthijn/dragoman/internal/core"
)

// Config is what the operator settled before the listener runs. There is no
// model, effort or cap here on purpose: those travel in the body, and a
// server-side default would make the same request mean different things at
// different instances.
type Config struct {
	Catalog  core.Catalog
	Executor core.Executor
	Strict   bool

	// Log carries what a request cost, what was dropped from it, and how it
	// failed. None of it reaches the answer: a nil logger discards the lot and
	// the caller reads the same body either way.
	Log *slog.Logger

	// LogHeaders is the allowlist of request headers the accounting line
	// records, in the order they are written. Empty records none — see
	// LoggedHeaders, which is where an operator's list becomes one.
	LogHeaders []string
}

// Service is one row of GET /services. It names the variable a service needs
// and never a key, and never whether one is set: an instance describes what it
// can address, not what it holds.
type Service struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Endpoint string `json:"endpoint"`
	Auth     string `json:"auth"`
}

// StatusBackendRefused answers a refusal the backend did not attach a status
// to — a provider error arriving in the body with a 200 on the status line.
// When there is a real status it is passed through instead, so that a rate
// limit keeps the retry semantics an SDK already implements.
const StatusBackendRefused = http.StatusBadGateway

// New builds the listener. It holds no state beyond these dependencies, so two
// instances serving the same table answer the same request the same way.
func New(cfg Config) http.Handler {
	listener := server{
		catalog:  cfg.Catalog,
		executor: cfg.Executor,
		strict:   cfg.Strict,
		log:      logger(cfg.Log),
		headers:  cfg.LogHeaders,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/responses", listener.only(http.MethodPost, listener.translate))
	mux.HandleFunc("/services", listener.only(http.MethodGet, listener.services))
	mux.HandleFunc("/health", listener.only(http.MethodGet, health))
	mux.HandleFunc("/", listener.unrouted)
	return mux
}

type server struct {
	catalog  core.Catalog
	executor core.Executor
	strict   bool
	log      *slog.Logger
	headers  []string
}

func (s server) translate(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, fmt.Sprintf("reading request body: %s", err))
		return
	}
	req, err := base.ParseRequest(body)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	named := base.GetModel(req)
	service, model, addressed := address(named)
	backend, served := s.catalog.Presets[service]
	if !addressed || !served {
		s.fail(w, r, http.StatusNotFound, fmt.Sprintf(
			"cannot resolve a service from model %q; a model names its service as its first "+
				"segment, and this instance serves: %s",
			named, strings.Join(serviceNames(s.catalog.Presets), ", ")))
		return
	}
	if !hasCredential(backend) {
		s.fail(w, r, http.StatusInternalServerError,
			fmt.Sprintf("service %q is unavailable: %s is not set", service, backend.KeyEnv))
		return
	}

	// The body already carries effort, cap and stream, so the pair the model
	// was read apart into, plus the server-wide strict flag, is the whole of
	// what the listener settles.
	opts := core.Options{Preset: service, Model: model, Strict: s.strict}
	clearWriteDeadline(w)
	if core.WantsEvents(req, opts) {
		s.streamed(w, r, opts, body)
		return
	}
	s.buffered(w, r, opts, body)
}

func (s server) buffered(w http.ResponseWriter, r *http.Request, opts core.Options, body []byte) {
	started := time.Now()
	result, err := core.Run(r.Context(), s.catalog, opts, body, s.executor, nil)
	elapsed := time.Since(started)
	s.record(opts.Preset, result.Drops)
	if err != nil {
		s.fail(w, r, statusOf(err), err.Error())
		return
	}
	encoded, err := json.Marshal(result.Response)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, fmt.Sprintf("encoding response: %s", err))
		return
	}
	writeJSON(w, http.StatusOK, encoded)
	s.served(r, opts, http.StatusOK, elapsed, result.Usage)
}

func (s server) streamed(w http.ResponseWriter, r *http.Request, opts core.Options, body []byte) {
	events := newStream(w)
	started := time.Now()
	result, err := core.Run(r.Context(), s.catalog, opts, body, s.executor, events.Send)
	elapsed := time.Since(started)
	s.record(opts.Preset, result.Drops)
	if err == nil {
		s.served(r, opts, http.StatusOK, elapsed, result.Usage)
		return
	}
	// The status line went out with the first event, so what is left to say has
	// to be said in the format the caller is already reading.
	if events.Started() {
		s.log.Error("stream failed", "service", opts.Preset, "error", err.Error())
		_ = events.Fail(responseError(statusOf(err), err.Error()))
		return
	}
	s.fail(w, r, statusOf(err), err.Error())
}

func (s server) services(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, encodeStrings(getServices(s.catalog.Presets)))
}

func (s server) unrouted(w http.ResponseWriter, r *http.Request) {
	s.fail(w, r, http.StatusNotFound, fmt.Sprintf("no route for %s; this instance serves "+
		"POST /responses, GET /services and GET /health", r.URL.Path))
}

// address reads the service off the front of a model and returns the name the
// backend is to be asked for. The split is on the first slash only, so a
// provider whose own identifiers are paths — openrouter spells one
// anthropic/claude-3 — keeps its identifier whole behind the service serving
// it. Both halves have to be there: a name that is all service or all model
// addresses nothing, and forwarding it would send the segment upstream as part
// of the model.
func address(named string) (service, model string, addressed bool) {
	service, model, split := strings.Cut(named, "/")
	return service, model, split && service != "" && model != ""
}

func health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []byte(liveness))
}

// only guards a route to one method. The listener answers a wrong method in
// the same error shape as everything else, because a client that can only read
// one format has to be able to read this too.
func (s server) only(method string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			s.fail(w, r, http.StatusMethodNotAllowed,
				fmt.Sprintf("%s is not allowed on %s; use %s", r.Method, r.URL.Path, method))
			return
		}
		next(w, r)
	}
}

// record puts drops in the server log and nowhere else. The response is the
// answer; a caller who wants the diff has the pipe. A warning is for the drops
// that changed the exchange: an inert drop repeated on every request is how an
// operator learns to skim past the warnings that matter, so it goes to debug.
func (s server) record(service string, drops []base.Drop) {
	notable, inert := base.PartitionDrops(drops)
	if len(notable) > 0 {
		s.log.Warn("dropped fields", "service", service, "drops", notable)
	}
	if len(inert) > 0 {
		s.log.Debug("dropped fields", "service", service, "drops", inert)
	}
}

// served is the accounting line: one record per request that worked, written
// where the money was spent. A translating proxy is the only place that knows
// both which service answered and what the answer cost, and a caller in front
// of it can log what it served without being able to say either.
//
// The duration is dragoman's exchange with the backend — translation, the call,
// and the decode — and not the caller's view of its own request, which includes
// a network hop dragoman cannot see.
func (s server) served(r *http.Request, opts core.Options, status int, elapsed time.Duration, usage *base.Usage) {
	fields := []any{
		"service", opts.Preset,
		"model", opts.Model,
		"status", status,
		"duration_ms", elapsed.Milliseconds(),
	}
	fields = append(fields, tokenCounts(usage)...)
	if headers := loggedHeaders(r.Header, s.headers); len(headers) > 0 {
		fields = append(fields, slog.GroupAttrs("headers", headers...))
	}
	s.log.Log(r.Context(), slog.LevelInfo, "request served", fields...)
}

// tokenCounts flattens the usage record into fields. The details hang off
// pointers a backend that reported none leaves nil, and every count is written
// anyway, at zero: a record whose key set depends on which provider answered
// cannot be summed across a day of them.
func tokenCounts(usage *base.Usage) []any {
	counted := base.Usage{}
	if usage != nil {
		counted = *usage
	}
	cached, reasoning := 0, 0
	if counted.InputTokenDetails != nil {
		cached = counted.InputTokenDetails.CachedTokens
	}
	if counted.OutputTokenDetails != nil {
		reasoning = counted.OutputTokenDetails.ReasoningTokens
	}
	return []any{
		"input_tokens", counted.InputTokens,
		"cached_tokens", cached,
		"output_tokens", counted.OutputTokens,
		"reasoning_tokens", reasoning,
		"total_tokens", counted.TotalTokens,
	}
}

func (s server) fail(w http.ResponseWriter, r *http.Request, status int, message string) {
	s.log.Log(r.Context(), levelFor(status), "request failed",
		"method", r.Method, "path", r.URL.Path, "status", status, "error", message)
	writeFailure(w, status, message)
}

// statusOf maps a run failure to a status. The unknown service and the unset
// credential never reach it: the core reports both as the same usage failure an
// unparseable body produces, so the listener decides those before it runs.
func statusOf(err error) int {
	switch code := core.CodeOf(err); code {
	case core.ExitOK:
		return http.StatusOK
	case core.ExitBackend:
		// Only a status a client can act on is worth passing through. Anything
		// outside the error range carries no retry semantics, and handing an
		// out-of-range number to WriteHeader panics the handler.
		if status := core.StatusOf(err); status >= 400 && status <= 599 {
			return status
		}
		return StatusBackendRefused
	case core.ExitUsage:
		return http.StatusBadRequest
	default:
		panic(fmt.Sprintf("serve: unknown exit code %d", code))
	}
}

// hasCredential asks the question core.Run asks again before it calls a
// backend. It is asked twice because the answer decides a status and the core
// cannot say which of its usage failures this was; a distinguishable failure in
// the core is the fix, not a check the transport keeps. A service that names no
// variable holds its own credential and has nothing to be missing.
func hasCredential(backend base.Backend) bool {
	return backend.KeyEnv == "" || os.Getenv(backend.KeyEnv) != ""
}

// getServices renders the table as data, sorted so an instance describes itself
// the same way twice.
func getServices(table map[string]base.Backend) []Service {
	rows := make([]Service, 0, len(table))
	for _, name := range serviceNames(table) {
		backend := table[name]
		rows = append(rows, Service{
			Name:     name,
			Protocol: backend.Dialect,
			Endpoint: backend.Endpoint,
			Auth:     backend.KeyEnv,
		})
	}
	return rows
}

func serviceNames(table map[string]base.Backend) []string {
	return slices.Sorted(maps.Keys(table))
}

// failureBody is an openai-responses error object.
type failureBody struct {
	Error base.ResponseError `json:"error"`
}

func writeFailure(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, encodeStrings(failureBody{Error: responseError(status, message)}))
}

func responseError(status int, message string) base.ResponseError {
	return base.ResponseError{Type: errorType(status), Message: message}
}

// errorType names the failure in the caller's vocabulary: a 5xx is dragoman's
// or the backend's, and everything else is the request's.
func errorType(status int) string {
	if status >= http.StatusInternalServerError {
		return "server_error"
	}
	return "invalid_request_error"
}

func writeJSON(w http.ResponseWriter, status int, encoded []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(encoded)
}

// encodeStrings renders a value whose fields are all strings. Such a value
// cannot fail to encode, so a failure here is a programmer error.
func encodeStrings(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic("serve: " + err.Error())
	}
	return encoded
}

// clearWriteDeadline keeps a long answer from being truncated by a deadline the
// server set for ordinary requests. Not every writer carries one.
func clearWriteDeadline(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
}

func levelFor(status int) slog.Level {
	if status >= http.StatusInternalServerError {
		return slog.LevelError
	}
	return slog.LevelWarn
}

func logger(supplied *slog.Logger) *slog.Logger {
	if supplied != nil {
		return supplied
	}
	return slog.New(slog.DiscardHandler)
}

const liveness = `{"status":"ok"}`
