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
	"sync"
	"time"

	"github.com/mdijkstra-oss/dragoman/internal/base"
	"github.com/mdijkstra-oss/dragoman/internal/core"
)

type Config struct {
	Catalog  core.Catalog
	Executor core.Executor
	Strict   bool

	Log *slog.Logger

	LogHeaders []string

	// Whether this instance stops short of the call. See TranslateOnlyVar.
	TranslateOnly bool
}

type Service struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Endpoint string `json:"endpoint"`
	Auth     string `json:"auth"`
	// Whether a request naming this service would reach its provider or answer
	// that the variable in Auth is unset. Read per request, so setting a key and
	// asking again reports the change without a restart.
	Available bool `json:"available"`
}

// For a provider error arriving in the body with a 200 on the status line. A
// real status is passed through instead, so a rate limit keeps the retry
// semantics an SDK already implements.
const StatusBackendRefused = http.StatusBadGateway

const streamKeepaliveInterval = 25 * time.Second

func New(cfg Config) http.Handler {
	listener := server{
		catalog:       cfg.Catalog,
		executor:      cfg.Executor,
		strict:        cfg.Strict,
		log:           logger(cfg.Log),
		headers:       cfg.LogHeaders,
		translateOnly: cfg.TranslateOnly,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/responses", listener.only(http.MethodPost, listener.translate))
	mux.HandleFunc("/services", listener.only(http.MethodGet, listener.services))
	mux.HandleFunc("/health", listener.only(http.MethodGet, health))
	mux.HandleFunc("/", listener.unrouted)
	return mux
}

type server struct {
	catalog       core.Catalog
	executor      core.Executor
	strict        bool
	log           *slog.Logger
	headers       []string
	translateOnly bool
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
	// Translating reaches no provider, so it needs no key. Demanding one would
	// put the translation out of reach on exactly the machine that holds none.
	if !s.translateOnly && !hasCredential(backend) {
		s.fail(w, r, http.StatusInternalServerError,
			fmt.Sprintf("service %q is unavailable: %s is not set", service, backend.KeyEnv))
		return
	}

	opts := core.Options{Preset: service, Model: model, Strict: s.strict, NoExecute: s.translateOnly}
	clearWriteDeadline(w)
	if s.translateOnly {
		s.translated(w, r, opts, body)
		return
	}
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

// translated answers with the body the backend would have received. NoExecute
// returns before the credential is read, so this cannot reach a provider and
// cannot spend anything. It is what --no-execute writes to stdout, over HTTP.
func (s server) translated(w http.ResponseWriter, r *http.Request, opts core.Options, body []byte) {
	started := time.Now()
	result, err := core.Run(r.Context(), s.catalog, opts, body, s.executor, nil)
	elapsed := time.Since(started)
	s.record(opts.Preset, result.Drops)
	if err != nil {
		s.fail(w, r, statusOf(err), err.Error())
		return
	}
	writeJSON(w, http.StatusOK, result.Sent)
	s.served(r, opts, http.StatusOK, elapsed, nil)
}

func (s server) streamed(w http.ResponseWriter, r *http.Request, opts core.Options, body []byte) {
	events := newStream(w)
	stopKeepalives := make(chan struct{})
	var keepalives sync.WaitGroup
	keepalives.Add(1)
	go func() {
		defer keepalives.Done()
		ticker := time.NewTicker(streamKeepaliveInterval)
		defer ticker.Stop()
		for {
			select {
			case <-r.Context().Done():
				return
			case <-stopKeepalives:
				return
			case <-ticker.C:
				if err := events.KeepAlive(); err != nil {
					return
				}
			}
		}
	}()
	started := time.Now()
	result, err := core.Run(r.Context(), s.catalog, opts, body, s.executor, events.Send)
	elapsed := time.Since(started)
	close(stopKeepalives)
	keepalives.Wait()
	s.record(opts.Preset, result.Drops)
	if err == nil {
		s.served(r, opts, http.StatusOK, elapsed, result.Usage)
		return
	}
	// The status line went out with the first event.
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

// The split is on the first slash only: OpenRouter's own identifiers are paths,
// spelled anthropic/claude-3, and have to stay whole behind the service serving
// them.
func address(named string) (service, model string, addressed bool) {
	service, model, split := strings.Cut(named, "/")
	return service, model, split && service != "" && model != ""
}

func health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []byte(liveness))
}

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

func (s server) record(service string, drops []base.Drop) {
	if len(drops) == 0 {
		return
	}
	s.log.Debug("dropped fields", "service", service, "drops", drops)
}

func (s server) served(r *http.Request, opts core.Options, status int, elapsed time.Duration, usage *base.Usage) {
	fields := []any{
		"service", opts.Preset,
		"model", opts.Model,
		"status", status,
		"duration_ms", elapsed.Milliseconds(),
	}
	fields = append(fields, tokenCounts(usage)...)
	if headers := headerAttrs(r.Header, s.headers); len(headers) > 0 {
		fields = append(fields, slog.GroupAttrs("headers", headers...))
	}
	s.log.Log(r.Context(), slog.LevelInfo, "request served", fields...)
}

// Every count is written even at zero: a record whose key set depends on which
// provider answered cannot be summed across a day of them.
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

func statusOf(err error) int {
	switch code := core.CodeOf(err); code {
	case core.ExitOK:
		return http.StatusOK
	case core.ExitBackend:
		// Handing an out-of-range number to WriteHeader panics the handler.
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

func hasCredential(backend base.Backend) bool {
	return backend.KeyEnv == "" || os.Getenv(backend.KeyEnv) != ""
}

func getServices(table map[string]base.Backend) []Service {
	rows := make([]Service, 0, len(table))
	for _, name := range serviceNames(table) {
		backend := table[name]
		rows = append(rows, Service{
			Name:      name,
			Protocol:  backend.Dialect,
			Endpoint:  backend.Endpoint,
			Auth:      backend.KeyEnv,
			Available: hasCredential(backend),
		})
	}
	return rows
}

func serviceNames(table map[string]base.Backend) []string {
	return slices.Sorted(maps.Keys(table))
}

type failureBody struct {
	Error base.ResponseError `json:"error"`
}

func writeFailure(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, encodeStrings(failureBody{Error: responseError(status, message)}))
}

func responseError(status int, message string) base.ResponseError {
	return base.ResponseError{Type: errorType(status), Message: message}
}

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

func encodeStrings(value any) []byte {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic("serve: " + err.Error())
	}
	return encoded
}

// A deadline set for ordinary requests would truncate a long answer. Not every
// writer carries one.
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
